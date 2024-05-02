/*
 * Copyright 2018-2020 the original author or authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *      https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package boot

import (
	"archive/zip"
	"fmt"
	"io/fs"

	"github.com/paketo-buildpacks/libpak/sherpa"

	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/buildpacks/libcnb"
	"github.com/magiconair/properties"
	"github.com/paketo-buildpacks/libpak"
	"github.com/paketo-buildpacks/libpak/bard"
	"github.com/paketo-buildpacks/libpak/effect"
)

type SpringPerformance struct {
	Dependency       libpak.BuildpackDependency
	LayerContributor libpak.LayerContributor
	Logger           bard.Logger
	Executor         effect.Executor
	AppPath          string
	Manifest         *properties.Properties
	AotEnabled       bool
	DoTrainingRun    bool
	ClasspathString  string
	ReZip            bool
}

func NewSpringPerformance(cache libpak.DependencyCache, appPath string, manifest *properties.Properties, aotEnabled bool, doTrainingRun bool, classpathString string, reZip bool) SpringPerformance {
	contributor := libpak.NewLayerContributor("Performance", cache, libcnb.LayerTypes{
		Build:  true,
		Launch: true,
	})
	return SpringPerformance{
		LayerContributor: contributor,
		Executor:         effect.NewExecutor(),
		AppPath:          appPath,
		Manifest:         manifest,
		AotEnabled:       aotEnabled,
		DoTrainingRun:    doTrainingRun,
		ClasspathString:  classpathString,
		ReZip:            reZip,
	}
}

func (s SpringPerformance) Contribute(layer libcnb.Layer) (libcnb.Layer, error) {
	s.LayerContributor.Logger = s.Logger
	layer, err := s.LayerContributor.Contribute(layer, func() (libcnb.Layer, error) {

		layer.LaunchEnvironment.Default("BPL_SPRING_AOT_ENABLED", s.AotEnabled)

		if !s.DoTrainingRun {
			return layer, nil
		}

		layer.LaunchEnvironment.Default("BPL_JVM_CDS_ENABLED", s.DoTrainingRun)

		// prepare the training run JVM opts
		var trainingRunArgs []string

		if s.AotEnabled {
			trainingRunArgs = append(trainingRunArgs, "-Dspring.aot.enabled=true")
		}

		jarPath := s.AppPath

		if s.ReZip {
			jarDestDir := os.TempDir() + "/" + fmt.Sprint(time.Now().UnixMilli()) + "/jar-dest"
			if err := os.MkdirAll(jarDestDir, 0755); err != nil {
				return layer, fmt.Errorf("error creating temp directory for jar\n%w", err)
			}
			tempJarPath := filepath.Join(jarDestDir, "runner.jar")
			if err := CreateJar(s.AppPath+"/", tempJarPath); err != nil {
				return layer, fmt.Errorf("error recreating jar\n%w", err)
			}
			f, err := os.Open(tempJarPath)
			if err != nil {
				return layer, fmt.Errorf("error opening jar\n%w", err)
			}
			if err = sherpa.CopyFile(f, filepath.Join(layer.Path, "runner.jar")); err != nil {
				return layer, fmt.Errorf("error copying jar\n%w", err)
			}

			jarPath = tempJarPath
			os.RemoveAll(s.AppPath)
		}

		if err := s.springBootJarCDSLayoutExtract(jarPath); err != nil {
			return layer, fmt.Errorf("error extracting Boot jar at %s\n%w", jarPath, err)
		}
		startClassValue, _ := s.Manifest.Get("Start-Class")

		if err := fs.WalkDir(os.DirFS(s.AppPath), ".", func(path string, d fs.DirEntry, err error) error {
			if time, err := time.Parse(time.DateTime, "1980-01-01 00:00:01"); err != nil {
				return fmt.Errorf("error parsing date-time\n%w", err)
			} else if err := os.Chtimes(path, time, time); err != nil {
				return fmt.Errorf("error resetting file times\n%w", err)
			}
			return nil
		}); err != nil {
			return libcnb.Layer{}, err
		}
		
		

		trainingRunArgs = append(trainingRunArgs,
			"-Dspring.context.exit=onRefresh",
			"-XX:ArchiveClassesAtExit=application.jsa",
			"-cp",
		)
		trainingRunArgs = append(trainingRunArgs, s.ClasspathString)
		trainingRunArgs = append(trainingRunArgs, startClassValue)

		var trainingRunEnvVariables []string

		javaToolOptions := sherpa.GetEnvWithDefault("CDS_TRAINING_JAVA_TOOL_OPTIONS", sherpa.GetEnvWithDefault("JAVA_TOOL_OPTIONS", ""))
		if javaToolOptions != "" {
			s.Logger.Bodyf("Training run will use this value as JAVA_TOOL_OPTIONS: %s", javaToolOptions)
			trainingRunEnvVariables = append(trainingRunEnvVariables, fmt.Sprintf("JAVA_TOOL_OPTIONS=%s", javaToolOptions))
		}

		// perform the training run, application.dsa, the cache file, will be created
		if err := s.Executor.Execute(effect.Execution{
			Command: "java",
			Env:     trainingRunEnvVariables,
			Args:    trainingRunArgs,
			Dir:     s.AppPath,
			Stdout:  s.Logger.InfoWriter(),
			Stderr:  s.Logger.InfoWriter(),
		}); err != nil {
			return libcnb.Layer{}, fmt.Errorf("error running build\n%w", err)
		}

		return layer, nil
	})

	if err != nil {
		return libcnb.Layer{}, fmt.Errorf("unable to contribute spring-cds layer\n%w", err)
	}
	return layer, nil
}

func (s SpringPerformance) Name() string {
	return s.LayerContributor.Name
}

func (s SpringPerformance) springBootJarCDSLayoutExtract(jarPath string) error {
	s.Logger.Bodyf("Extracting Jar")
	if err := s.Executor.Execute(effect.Execution{
		Command: "java",
		Args:    []string{"-Djarmode=tools", "-jar", jarPath, "extract", "--destination", s.AppPath},
		Dir:     filepath.Dir(jarPath),
		Stdout:  s.Logger.InfoWriter(),
		Stderr:  s.Logger.InfoWriter(),
	}); err != nil {
		return fmt.Errorf("error extracting Jar with jarmode\n%w", err)
	}
	return nil
}

// CreateJar heavily inspired by: https://gosamples.dev/zip-file/
func CreateJar(source, target string) error {

	// 1. Create a ZIP file and zip.Writer
	f, err := os.Create(target)
	if err != nil {
		return err
	}
	defer f.Close()

	writer := zip.NewWriter(f)
	defer writer.Close()

	// 2. Go through all the files of the source
	return filepath.Walk(source, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		absolutePath := ""

		if info.Mode()&os.ModeSymlink == os.ModeSymlink {
			if absolutePath, err = filepath.EvalSymlinks(path); err != nil {
				return fmt.Errorf("unable to eval symlink %s\n%w", absolutePath, err)
			}
			if file, err := os.Open(absolutePath); err != nil {
				return fmt.Errorf("unable to open %s\n%w", absolutePath, err)
			} else {
				if info, err = file.Stat(); err != nil {
					return fmt.Errorf("unable to stat %s\n%w", absolutePath, err)
				}
			}
		}

		// 3. Create a local file header
		header, err := zip.FileInfoHeader(info)
		if err != nil {
			return err
		}

		// set compression
		header.Method = zip.Store
		// 4. Set relative path of a file as the header name
		header.Name, err = filepath.Rel(source, path)
		if err != nil {
			return err
		}
		if info.IsDir() {
			header.Name += "/"
		}

		// 5. Create writer for the file header and save content of the file
		headerWriter, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}

		if info.IsDir() {
			return nil
		}

		if absolutePath != "" {
			path = absolutePath
		}

		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()

		_, err = io.Copy(headerWriter, f)
		writer.Flush()
		return err
	})

}
