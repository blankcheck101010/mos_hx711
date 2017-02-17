package main

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/hex"
	"fmt"
	"io"
	"io/ioutil"
	"mime/multipart"
	"net/http"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"strings"

	"cesanta.com/cloud/common/ide"
	"cesanta.com/cloud/common/swmodule"
	"cesanta.com/cloud/util/archive"
	"cesanta.com/common/go/multierror"
	"cesanta.com/mos/dev"
	"cesanta.com/mos/flash/common"
	"github.com/cesanta/errors"
	"github.com/golang/glog"
	flag "github.com/spf13/pflag"
	yaml "gopkg.in/yaml.v2"
)

// mos build specific advanced flags
var (
	buildImages = flag.String("docker_images",
		"esp8266=docker.cesanta.com/mg-iot-cloud-project-esp8266:release,"+
			"cc3200=docker.cesanta.com/mg-iot-cloud-project-cc3200:release",
		"build images, arch1=image1,arch2=image2")
	cleanBuild     = flag.Bool("clean", false, "perform a clean build, wipe the previous build state")
	modules        = flag.StringSlice("module", []string{}, "location of the module from mos.yaml, in the format: \"module_name:/path/to/location\". Can be used multiple times.")
	buildVarsSlice = flag.StringSlice("build_var", []string{}, "build variable in the format \"NAME:VALUE\" Can be used multiple times.")
)

const (
	buildDir = "build"
	codeDir  = "."

	buildLog = "build.log"
)

func init() {
	hiddenFlags = append(hiddenFlags, "docker_images")
}

func build(ctx context.Context, devConn *dev.DevConn) error {
	var err error
	if *local {
		err = buildLocal()
	} else {
		err = buildRemote()
	}
	if err != nil {
		return errors.Trace(err)
	}

	fwFilename := filepath.Join(buildDir, ide.FirmwareFileName)

	fw, err := common.NewZipFirmwareBundle(fwFilename)
	if err == nil {
		fmt.Printf("Success, built %s/%s version %s (%s).\n", fw.Name, fw.Platform, fw.Version, fw.BuildID)
	}

	fmt.Printf("Firmware saved to %s\n", fwFilename)

	return err
}

func buildLocal() (err error) {
	defer func() {
		if !*verbose && err != nil {
			log, err := os.Open(path.Join(buildDir, buildLog))
			if err != nil {
				glog.Errorf("can't read build log: %s", err)
				return
			}
			io.Copy(os.Stdout, log)
		}
	}()

	fwDir := filepath.Join(buildDir, "fw")
	objsDir := filepath.Join(buildDir, "objs")
	genDir := filepath.Join(buildDir, "gen")
	fsDir := filepath.Join(buildDir, "fs")
	fwFilename := filepath.Join(buildDir, ide.FirmwareFileName)

	if *cleanBuild {
		err = os.RemoveAll(buildDir)
		if err != nil {
			return errors.Trace(err)
		}
	} else {
		// This is not going to be a clean build, but we should still remove fw.zip
		// (ignoring any possible errors)
		os.Remove(fwFilename)
	}

	err = os.MkdirAll(buildDir, 0777)
	if err != nil {
		return errors.Trace(err)
	}

	blog := filepath.Join(buildDir, buildLog)
	logFile, err := os.Create(blog)
	if err != nil {
		return errors.Trace(err)
	}
	defer logFile.Close()

	manifest, err := readManifest()
	if err != nil {
		return errors.Trace(err)
	}

	// Create map of given module locations, via --module flag(s)
	moduleLocations := map[string]string{}
	for _, m := range *modules {
		parts := strings.SplitN(m, ":", 2)
		moduleLocations[parts[0]] = parts[1]
	}

	appModules := manifest.Sources

	var mosDirEffective string
	if *mosRepo != "" {
		fmt.Printf("Using mongoose-os located at %q\n", *mosRepo)
		mosDirEffective = *mosRepo
	} else {
		fmt.Printf("The flag --repo is not given, going to use mongoose-os repository\n")
		mosDirEffective = "mongoose-os"

		m := swmodule.SWModule{
			Type: "git",
			// TODO(dfrank) get upstream repo URL from a flag
			// (and this flag needs to be forwarded to fwbuild as well, which should
			// forward it to the mos invocation)
			Src:     "https://github.com/cesanta/mongoose-os",
			Version: manifest.MongooseOsVersion,
		}

		if err := m.PrepareLocalCopy(mosDirEffective, logFile, true); err != nil {
			return errors.Trace(err)
		}
	}

	for _, m := range manifest.Modules {
		name, err := m.GetName()
		if err != nil {
			return errors.Trace(err)
		}

		targetDir, ok := moduleLocations[name]
		if !ok {
			// Custom module location wasn't provided in command line, so, we'll
			// use the module name and will clone/pull it if necessary
			fmt.Printf("The flag --module is not given for the module %q, going to use the repository\n", name)
			targetDir = name

			if err := m.PrepareLocalCopy(targetDir, logFile, true); err != nil {
				return errors.Trace(err)
			}
		} else {
			fmt.Printf("Using module %q located at %q\n", name, targetDir)
		}

		appModules = append(appModules, targetDir)
	}

	ffiSymbols := manifest.FFISymbols

	fmt.Printf("Building...\n")

	archEffective, err := detectArch(manifest)
	if err != nil {
		return errors.Trace(err)
	}

	defer os.RemoveAll(fwDir)

	appName, err := fixupAppName(manifest.Name)
	if err != nil {
		return errors.Trace(err)
	}

	var errs error
	for k, v := range map[string]string{
		"MGOS_PATH":      mosDirEffective,
		"PLATFORM":       archEffective,
		"BUILD_DIR":      objsDir,
		"FW_DIR":         fwDir,
		"GEN_DIR":        genDir,
		"FS_STAGING_DIR": fsDir,
		"APP":            appName,
		"APP_VERSION":    manifest.Version,
		"APP_MODULES":    strings.Join(appModules, " "),
		"APP_FS_PATH":    strings.Join(manifest.Filesystem, " "),
		"FFI_SYMBOLS":    strings.Join(ffiSymbols, " "),
	} {
		err := addBuildVar(manifest, k, v)
		if err != nil {
			errs = multierror.Append(errs, err)
		}
	}
	if errs != nil {
		return errors.Trace(errs)
	}

	// Add build vars from CLI flags
	for _, v := range *buildVarsSlice {
		parts := strings.SplitN(v, ":", 2)
		manifest.BuildVars[parts[0]] = parts[1]
	}

	makeArgs := []string{
		"-j",
		"-f", filepath.Join(mosDirEffective, "fw/platforms", archEffective, "Makefile"),
	}
	for k, v := range manifest.BuildVars {
		makeArgs = append(makeArgs, fmt.Sprintf("%s=%s", k, v))
	}

	if *verbose {
		fmt.Printf("Make arguments: %s\n", strings.Join(makeArgs, " "))
	}

	cmd := exec.Command("make", makeArgs...)
	err = runCmd(cmd, logFile)
	if err != nil {
		return errors.Trace(err)
	}

	// Move firmware as build/fw.zip
	err = os.Rename(
		filepath.Join(fwDir, fmt.Sprintf("%s-%s-last.zip", appName, archEffective)),
		fwFilename,
	)
	if err != nil {
		return errors.Trace(err)
	}

	return nil
}

// addBuildVar adds a given build variable to manifest.BuildVars,
// but if the variable already exists, returns an error.
func addBuildVar(manifest *ide.FWAppManifest, name, value string) error {
	if _, ok := manifest.BuildVars[name]; ok {
		return errors.Errorf(
			"Build variable %q should not be given in %q "+
				"since it's set by the mos tool automatically",
			name, ide.ManifestFileName,
		)
	}
	manifest.BuildVars[name] = value
	return nil
}

// runCmd runs given command and redirects its output to the given log file.
// if --verbose flag is set, then the output also goes to the stdout.
func runCmd(cmd *exec.Cmd, logFile io.Writer) error {
	writers := []io.Writer{logFile}
	if *verbose {
		writers = append(writers, os.Stdout)
	}
	out := io.MultiWriter(writers...)
	cmd.Stdout = out
	cmd.Stderr = out
	err := cmd.Run()
	if err != nil {
		return errors.Trace(err)
	}
	return nil
}

func detectArch(manifest *ide.FWAppManifest) (string, error) {
	a := *arch
	if a == "" {
		a = manifest.Arch
	}

	if a == "" {
		return "", errors.Errorf("--arch must be specified or mos.yml should contain an arch key")
	}
	return a, nil
}

func readManifest() (*ide.FWAppManifest, error) {
	manifestSrc, err := ioutil.ReadFile(filepath.Join(codeDir, ide.ManifestFileName))
	if err != nil {
		return nil, errors.Trace(err)
	}

	var manifest ide.FWAppManifest
	if err := yaml.Unmarshal(manifestSrc, &manifest); err != nil {
		return nil, errors.Trace(err)
	}

	if manifest.BuildVars == nil {
		manifest.BuildVars = make(map[string]string)
	}

	if manifest.MongooseOsVersion == "" {
		manifest.MongooseOsVersion = "master"
	}

	return &manifest, nil
}

func buildRemote() error {
	manifest, err := readManifest()
	if err != nil {
		return errors.Trace(err)
	}

	whitelist := map[string]bool{
		ide.ManifestFileName: true,
		".":                  true,
	}
	for _, v := range manifest.Sources {
		whitelist[v] = true
	}
	for _, v := range manifest.Filesystem {
		whitelist[v] = true
	}
	for _, v := range manifest.ExtraFiles {
		whitelist[v] = true
	}

	transformers := make(map[string]fileTransformer)

	// We need to preprocess mos.yml (see setManifestArch())
	transformers[ide.ManifestFileName] = func(r io.ReadCloser) (io.ReadCloser, error) {
		var buildVars map[string]string
		if len(*buildVarsSlice) > 0 {
			buildVars = make(map[string]string)
			for _, v := range *buildVarsSlice {
				parts := strings.SplitN(v, ":", 2)
				buildVars[parts[0]] = parts[1]
			}
		}
		return setManifestArch(r, *arch, buildVars)
	}

	// create a zip out of the current dir
	src, err := zipUp(".", whitelist, transformers)
	if err != nil {
		return errors.Trace(err)
	}
	if glog.V(2) {
		glog.V(2).Infof("zip:", hex.Dump(src))
	}

	// prepare multipart body
	body := &bytes.Buffer{}
	mpw := multipart.NewWriter(body)
	part, err := mpw.CreateFormFile("file", "source.zip")
	if err != nil {
		return errors.Trace(err)
	}

	if _, err := part.Write(src); err != nil {
		return errors.Trace(err)
	}
	if err := mpw.Close(); err != nil {
		return errors.Trace(err)
	}

	server, err := serverURL()
	if err != nil {
		return errors.Trace(err)
	}

	buildUser := "test"
	buildPass := "test"
	fmt.Printf("Connecting to %s, user %s\n", server, buildUser)

	// invoke the fwbuild API
	uri := fmt.Sprintf("%s/api/%s/firmware/build", server, buildUser)

	fmt.Printf("Uploading sources (%d bytes)\n", len(body.Bytes()))
	req, err := http.NewRequest("POST", uri, body)
	req.Header.Set("Content-Type", mpw.FormDataContentType())
	req.SetBasicAuth(buildUser, buildPass)

	client := &http.Client{}
	resp, err := client.Do(req)
	if err != nil {
		return errors.Trace(err)
	}

	// handle response
	body.Reset()
	body.ReadFrom(resp.Body)

	switch resp.StatusCode {
	case http.StatusOK, http.StatusTeapot:
		// Build either succeeded or failed

		// unzip build results
		r := bytes.NewReader(body.Bytes())
		os.RemoveAll(buildDir)
		archive.UnzipInto(r, r.Size(), ".", 0)

		// print log in verbose mode or when build fails
		if *verbose || resp.StatusCode != http.StatusOK {
			log, err := os.Open(path.Join(buildDir, buildLog))
			if err != nil {
				return errors.Trace(err)
			}
			io.Copy(os.Stdout, log)
		}

		if resp.StatusCode != http.StatusOK {
			return errors.Errorf("build failed")
		}
		return nil

	default:
		// Unexpected response
		return errors.Errorf("error response: %d: ", resp.StatusCode, strings.TrimSpace(body.String()))
	}

}

type fileTransformer func(r io.ReadCloser) (io.ReadCloser, error)

// zipUp takes the whitelisted files and directories under path and returns an
// in-memory zip file. The whitelist map is applied to top-level dirs and files
// only. If some file needs to be transformed before placing into a zip
// archive, the appropriate transformer function should be placed at the
// transformers map.
func zipUp(
	dir string,
	whitelist map[string]bool,
	transformers map[string]fileTransformer,
) ([]byte, error) {
	data := &bytes.Buffer{}
	z := zip.NewWriter(data)

	err := filepath.Walk(dir, func(file string, info os.FileInfo, err error) error {
		// Zip files should always contain forward slashes
		fileForwardSlash := file
		if os.PathSeparator != rune('/') {
			fileForwardSlash = strings.Replace(file, string(os.PathSeparator), "/", -1)
		}
		parts := strings.Split(file, string(os.PathSeparator))
		if _, ok := whitelist[parts[0]]; !ok {
			glog.Infof("ignoring %q", file)
			if info.IsDir() {
				return filepath.SkipDir
			} else {
				return nil
			}
		}
		if info.IsDir() {
			return nil
		}

		glog.Infof("zipping %s", file)

		w, err := z.Create(path.Join("src", fileForwardSlash))
		if err != nil {
			return errors.Trace(err)
		}

		var r io.ReadCloser
		r, err = os.Open(file)
		if err != nil {
			return errors.Trace(err)
		}
		defer r.Close()

		t, ok := transformers[fileForwardSlash]
		if !ok {
			t = identityTransformer
		}

		r, err = t(r)
		if err != nil {
			return errors.Trace(err)
		}
		defer r.Close()

		if _, err := io.Copy(w, r); err != nil {
			return errors.Trace(err)
		}

		return nil
	})
	if err != nil {
		return nil, errors.Trace(err)
	}

	z.Close()
	return data.Bytes(), nil
}

func fixupAppName(appName string) (string, error) {
	if appName == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", errors.Trace(err)
		}
		return filepath.Base(wd), nil
	}
	return appName, nil
}

// setManifestArch takes manifest data, replaces architecture with the given
// value if it's not empty, sets app name to the current directory name if
// original value is empty, and returns resulting manifest data
func setManifestArch(
	r io.ReadCloser, arch string, buildVars map[string]string,
) (io.ReadCloser, error) {
	manifestData, err := ioutil.ReadAll(r)
	if err != nil {
		return nil, errors.Trace(err)
	}

	var manifest ide.FWAppManifest
	if err := yaml.Unmarshal(manifestData, &manifest); err != nil {
		return nil, errors.Trace(err)
	}

	if arch != "" {
		manifest.Arch = arch
	}

	if buildVars != nil {
		if manifest.BuildVars == nil {
			manifest.BuildVars = make(map[string]string)
		}
		for k, v := range buildVars {
			manifest.BuildVars[k] = v
		}
	}

	manifest.Name, err = fixupAppName(manifest.Name)
	if err != nil {
		return nil, errors.Trace(err)
	}

	manifestData, err = yaml.Marshal(&manifest)
	if err != nil {
		return nil, errors.Trace(err)
	}

	return struct {
		io.Reader
		io.Closer
	}{bytes.NewReader(manifestData), r}, nil
}

func identityTransformer(r io.ReadCloser) (io.ReadCloser, error) {
	return r, nil
}
