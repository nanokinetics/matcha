// Copyright 2014 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package cmd

import (
	"archive/zip"
	"fmt"
	"go/build"
	"io"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

const (
	javacTargetVer = "1.7"
	minAndroidAPI  = 15
)

const manifestHeader = `Manifest-Version: 1.0
Created-By: 1.0 (Go)

`

func androidHostTag() (string, error) {
	if runtime.GOOS == "windows" && runtime.GOARCH == "386" {
		return "windows", nil
	} else {
		var arch string
		switch runtime.GOARCH {
		case "386":
			arch = "x86"
		case "amd64":
			arch = "x86_64"
		default:
			return "", fmt.Errorf("androidHostTag(): Unsupported GOARCH: %v", runtime.GOARCH)
		}
		return runtime.GOOS + "-" + arch, nil
	}
}

func ndkRoot() (string, error) {
	sdkHome := os.Getenv("ANDROID_HOME")
	if sdkHome == "" {
		return "", fmt.Errorf("$ANDROID_HOME does not point to an Android NDK. $ANDROID_HOME is unset.")
	}

	path, err := filepath.Abs(filepath.Join(sdkHome, "ndk-bundle"))
	if err != nil {
		return "", fmt.Errorf("$ANDROID_HOME does not point to an Android NDK. Error cleaning path %v.", err)
	}

	if st, err := os.Stat(path); err != nil || !st.IsDir() {
		return "", fmt.Errorf("$ANDROID_HOME does not point to an Android NDK. Missing directory at %v.", path)
	}
	return path, nil
}

// Emulate the flags in the clang wrapper scripts generated
// by make_standalone_toolchain.py
type ndkToolchain struct {
	arch        string
	abi         string
	platform    string
	gcc         string
	toolPrefix  string
	clangTarget string
	// Computed
	ndkRoot string
	hostTag string
}

func toolchainForArch(goarch string) (*ndkToolchain, error) {
	m := map[string]*ndkToolchain{
		"arm": &ndkToolchain{
			arch:        "arm",
			platform:    "android-15",
			gcc:         "arm-linux-androideabi-4.9",
			toolPrefix:  "arm-linux-androideabi",
			clangTarget: "armv7a-none-linux-androideabi",
		},
		"arm64": &ndkToolchain{
			arch:        "arm64",
			platform:    "android-21",
			gcc:         "aarch64-linux-android-4.9",
			toolPrefix:  "aarch64-linux-android",
			clangTarget: "aarch64-none-linux-android",
		},
		"386": &ndkToolchain{
			arch:        "x86",
			platform:    "android-15",
			gcc:         "x86-4.9",
			toolPrefix:  "i686-linux-android",
			clangTarget: "i686-none-linux-android",
		},
		"amd64": &ndkToolchain{
			arch:        "x86_64",
			platform:    "android-21",
			gcc:         "x86_64-4.9",
			toolPrefix:  "x86_64-linux-android",
			clangTarget: "x86_64-none-linux-android",
		},
	}
	toolchain, ok := m[goarch]
	if !ok {
		return nil, fmt.Errorf("toolchainForArch(): Unknown arch %v", goarch)
	}

	ndkRoot, err := ndkRoot()
	if err != nil {
		return nil, err
	}
	toolchain.ndkRoot = ndkRoot

	hostTag, err := androidHostTag()
	if err != nil {
		return nil, err
	}
	toolchain.hostTag = hostTag
	return toolchain, nil
}

func (tc *ndkToolchain) gccToolchainPath() string {
	return filepath.Join(tc.ndkRoot, "toolchains", tc.gcc, "prebuilt", tc.hostTag)
}

func (tc *ndkToolchain) clangPath() string {
	return filepath.Join(tc.ndkRoot, "toolchains", "llvm", "prebuilt", tc.hostTag, "bin", "clang")
}

func (tc *ndkToolchain) clangppPath() string {
	return filepath.Join(tc.ndkRoot, "toolchains", "llvm", "prebuilt", tc.hostTag, "bin", "clang++")
}

func (tc *ndkToolchain) sysroot() string {
	return filepath.Join(tc.ndkRoot, "platforms", tc.platform, "arch-"+tc.arch)
}

func GetAndroidABI(arch string) string {
	switch arch {
	case "arm":
		return "armeabi-v7a"
	case "arm64":
		return "arm64-v8a"
	case "386":
		return "x86"
	case "amd64":
		return "x86_64"
	}
	return ""
}

func androidEnv(goarch string) ([]string, error) {
	tc, err := toolchainForArch(goarch)
	if err != nil {
		return nil, err
	}
	flags := fmt.Sprintf("-target %s --sysroot %s -gcc-toolchain %s", tc.clangTarget, tc.sysroot(), tc.gccToolchainPath())
	cflags := fmt.Sprintf("%s", flags)
	ldflags := fmt.Sprintf("%s -L%s/usr/lib", flags, tc.sysroot())
	env := []string{
		"GOOS=android",
		"GOARCH=" + goarch,
		"CC=" + tc.clangPath(),
		"CXX=" + tc.clangppPath(),
		"CGO_CFLAGS=" + cflags,
		"CGO_CPPFLAGS=" + cflags,
		"CGO_LDFLAGS=" + ldflags,
		"CGO_ENABLED=1",
	}
	if goarch == "arm" {
		env = append(env, "GOARM=7")
	}
	return env, nil
}

// androidAPIPath returns an android SDK platform directory under ANDROID_HOME.
// If there are multiple platforms that satisfy the minimum version requirement
// androidAPIPath returns the latest one among them.
func AndroidAPIPath() (string, error) {
	sdk := os.Getenv("ANDROID_HOME")
	if sdk == "" {
		return "", fmt.Errorf("ANDROID_HOME environment var is not set")
	}
	sdkDir, err := os.Open(filepath.Join(sdk, "platforms"))
	if err != nil {
		return "", fmt.Errorf("failed to find android SDK platform: %v", err)
	}
	defer sdkDir.Close()
	fis, err := sdkDir.Readdir(-1)
	if err != nil {
		return "", fmt.Errorf("failed to find android SDK platform (min API level: %d): %v", minAndroidAPI, err)
	}

	var apiPath string
	var apiVer int
	for _, fi := range fis {
		name := fi.Name()
		if !fi.IsDir() || !strings.HasPrefix(name, "android-") {
			continue
		}
		n, err := strconv.Atoi(name[len("android-"):])
		if err != nil || n < minAndroidAPI {
			continue
		}
		p := filepath.Join(sdkDir.Name(), name)
		_, err = os.Stat(filepath.Join(p, "android.jar"))
		if err == nil && apiVer < n {
			apiPath = p
			apiVer = n
		}
	}
	if apiVer == 0 {
		return "", fmt.Errorf("failed to find android SDK platform (min API level: %d) in %s",
			minAndroidAPI, sdkDir.Name())
	}
	return apiPath, nil
}

// AAR is the format for the binary distribution of an Android Library Project
// and it is a ZIP archive with extension .aar.
// http://tools.android.com/tech-docs/new-build-system/aar-format
//
// These entries are directly at the root of the archive.
//
//  AndroidManifest.xml (mandatory)
//  classes.jar (mandatory)
//  assets/ (optional)
//  jni/<abi>/libgojni.so
//  R.txt (mandatory)
//  res/ (mandatory)
//  libs/*.jar (optional, not relevant)
//  proguard.txt (optional)
//  lint.jar (optional, not relevant)
//  aidl (optional, not relevant)
//
// javac and jar commands are needed to build classes.jar.
func BuildAAR(flags *Flags, androidDir string, pkgs []*build.Package, androidArchs []string, tmpdir string, aarPath string) (err error) {
	var out io.Writer = ioutil.Discard
	if !flags.BuildN {
		f, err := os.Create(aarPath)
		if err != nil {
			return err
		}
		defer func() {
			if cerr := f.Close(); err == nil {
				err = cerr
			}
		}()
		out = f
	}

	aarw := zip.NewWriter(out)
	aarwcreate := func(name string) (io.Writer, error) {
		if flags.BuildV {
			fmt.Fprintf(os.Stderr, "aar: %s\n", name)
		}
		return aarw.Create(name)
	}
	w, err := aarwcreate("AndroidManifest.xml")
	if err != nil {
		return err
	}
	const manifestFmt = `<manifest xmlns:android="http://schemas.android.com/apk/res/android" package=%q>
<uses-sdk android:minSdkVersion="%d"/></manifest>`
	fmt.Fprintf(w, manifestFmt, "go."+pkgs[0].Name+".gojni", minAndroidAPI)

	w, err = aarwcreate("proguard.txt")
	if err != nil {
		return err
	}
	fmt.Fprintln(w, `-keep class go.** { *; }`)

	w, err = aarwcreate("classes.jar")
	if err != nil {
		return err
	}
	src := filepath.Join(androidDir, "src/main/java")
	if err := BuildJar(flags, w, src, tmpdir); err != nil {
		return err
	}

	files := map[string]string{}
	for _, pkg := range pkgs {
		assetsDir := filepath.Join(pkg.Dir, "assets")
		assetsDirExists := false
		if fi, err := os.Stat(assetsDir); err == nil {
			assetsDirExists = fi.IsDir()
		} else if !os.IsNotExist(err) {
			return err
		}

		if assetsDirExists {
			err := filepath.Walk(
				assetsDir, func(path string, info os.FileInfo, err error) error {
					if err != nil {
						return err
					}
					if info.IsDir() {
						return nil
					}
					f, err := os.Open(path)
					if err != nil {
						return err
					}
					defer f.Close()
					name := "assets/" + path[len(assetsDir)+1:]
					if orig, exists := files[name]; exists {
						return fmt.Errorf("package %s asset name conflict: %s already added from package %s",
							pkg.ImportPath, name, orig)
					}
					files[name] = pkg.ImportPath
					w, err := aarwcreate(name)
					if err != nil {
						return nil
					}
					_, err = io.Copy(w, f)
					return err
				})
			if err != nil {
				return err
			}
		}
	}

	for _, arch := range androidArchs {
		lib := GetAndroidABI(arch) + "/libgojni.so"
		w, err = aarwcreate("jni/" + lib)
		if err != nil {
			return err
		}
		if !flags.BuildN {
			r, err := os.Open(filepath.Join(androidDir, "src/main/jniLibs/"+lib))
			if err != nil {
				return err
			}
			defer r.Close()
			if _, err := io.Copy(w, r); err != nil {
				return err
			}
		}
	}

	// TODO(hyangah): do we need to use aapt to create R.txt?
	w, err = aarwcreate("R.txt")
	if err != nil {
		return err
	}

	w, err = aarwcreate("res/")
	if err != nil {
		return err
	}

	return aarw.Close()
}

func BuildJar(flags *Flags, w io.Writer, srcDir string, tmpdir string) error {
	bindClasspath := ""

	var srcFiles []string
	if flags.BuildN {
		srcFiles = []string{"*.java"}
	} else {
		err := filepath.Walk(srcDir, func(path string, info os.FileInfo, err error) error {
			if err != nil {
				return err
			}
			if filepath.Ext(path) == ".java" {
				srcFiles = append(srcFiles, filepath.Join(".", path[len(srcDir):]))
			}
			return nil
		})
		if err != nil {
			return err
		}
	}

	dst := filepath.Join(tmpdir, "javac-output")
	if !flags.BuildN {
		if err := os.MkdirAll(dst, 0700); err != nil {
			return err
		}
	}

	bClspath, err := bootClasspath()

	if err != nil {
		return err
	}

	args := []string{
		"-d", dst,
		"-source", javacTargetVer,
		"-target", javacTargetVer,
		"-bootclasspath", bClspath,
	}
	if bindClasspath != "" {
		args = append(args, "-classpath", bindClasspath)
	}

	args = append(args, srcFiles...)

	javac := exec.Command("javac", args...)
	javac.Dir = srcDir
	if err := RunCmd(&Flags{}, tmpdir, javac); err != nil {
		return err
	}

	// fmt.Println("javac", args)
	// if buildX {
	// KD: printcmd("jar c -C %s .", dst)
	// }
	if flags.BuildN {
		return nil
	}
	jarw := zip.NewWriter(w)
	jarwcreate := func(name string) (io.Writer, error) {
		if flags.BuildV {
			fmt.Fprintf(os.Stderr, "jar: %s\n", name)
		}
		return jarw.Create(name)
	}
	f, err := jarwcreate("META-INF/MANIFEST.MF")
	if err != nil {
		return err
	}
	fmt.Fprintf(f, manifestHeader)

	err = filepath.Walk(dst, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		out, err := jarwcreate(filepath.ToSlash(path[len(dst)+1:]))
		if err != nil {
			return err
		}
		in, err := os.Open(path)
		if err != nil {
			return err
		}
		defer in.Close()
		_, err = io.Copy(out, in)
		return err
	})
	if err != nil {
		return err
	}
	return jarw.Close()
}

func bootClasspath() (string, error) {
	// bindBootClasspath := "" // KD: command parameter
	// if bindBootClasspath != "" {
	// 	return bindBootClasspath, nil
	// }
	apiPath, err := AndroidAPIPath()
	if err != nil {
		return "", err
	}
	return filepath.Join(apiPath, "android.jar"), nil
}
