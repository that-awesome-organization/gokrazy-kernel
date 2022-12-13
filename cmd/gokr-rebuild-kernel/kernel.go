package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"os/user"
	"path"
	"path/filepath"
	"strings"
	"text/template"
)

var (
	latestVersion = "latest"

	releasesURL                  = "https://www.kernel.org/releases.json"
	buildPath                    = flag.String("build-path", "cmd/gokr-build-kernel", "Build Package path")
	dobuild                      = flag.Bool("enable-build", false, "Enables building the kernel as well")
	overwriteContainerExecutable = flag.String("overwrite_container_executable",
		"",
		"E.g. docker or podman to overwrite the automatically detected container executable")

	urlTemplate = `
package main

// see https://www.kernel.org/releases.json
var latest = "{{ . }}"
`
)

const dockerFileContents = `
FROM debian:stretch

RUN apt-get update && apt-get install -y crossbuild-essential-arm64 bc libssl-dev bison flex kmod

COPY gokr-build-kernel /usr/bin/gokr-build-kernel
{{- range $idx, $path := .Patches }}
COPY {{ $path }} /usr/src/{{ $path }}
{{- end }}

RUN echo 'builduser:x:{{ .Uid }}:{{ .Gid }}:nobody:/:/bin/sh' >> /etc/passwd && \
    chown -R {{ .Uid }}:{{ .Gid }} /usr/src

USER builduser
WORKDIR /usr/src
ENTRYPOINT /usr/bin/gokr-build-kernel
`

var dockerFileTmpl = template.Must(template.New("dockerfile").
	Funcs(map[string]interface{}{
		"basename": func(path string) string {
			return filepath.Base(path)
		},
	}).
	Parse(dockerFileContents))

var patchFiles = []string{
	"0001-Revert-add-index-to-the-ethernet-alias.patch",
	// spi
	"0201-enable-spidev.patch",
	// logo
	"0001-gokrazy-logo.patch",
}

func copyFile(dest, src string) error {
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	defer out.Close()

	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()

	if _, err := io.Copy(out, in); err != nil {
		return err
	}

	st, err := in.Stat()
	if err != nil {
		return err
	}
	if err := out.Chmod(st.Mode()); err != nil {
		return err
	}
	return out.Close()
}

var gopath = mustGetGopath()

func mustGetGopath() string {
	gopathb, err := exec.Command("go", "env", "GOPATH").Output()
	if err != nil {
		log.Panic(err)
	}
	return strings.TrimSpace(string(gopathb))
}

func find(filename string) (string, error) {
	if _, err := os.Stat(filename); err == nil {
		return filename, nil
	}

	path := filepath.Join(gopath, "src", "development.thatwebsite.xyz", "gokrazy", "kernel", filename)
	if _, err := os.Stat(path); err == nil {
		return path, nil
	}

	return "", fmt.Errorf("could not find file %q (looked in . and %s)", filename, path)
}

func getContainerExecutable() (string, error) {
	// Probe podman first, because the docker binary might actually
	// be a thin podman wrapper with podman behavior.
	choices := []string{"podman", "docker"}
	for _, exe := range choices {
		p, err := exec.LookPath(exe)
		if err != nil {
			continue
		}
		resolved, err := filepath.EvalSymlinks(p)
		if err != nil {
			return "", err
		}
		return resolved, nil
	}
	return "", fmt.Errorf("none of %v found in $PATH", choices)
}

func main() {
	flag.Parse()

	log.SetFlags(log.LstdFlags | log.Lshortfile)
	err := updateVersion()
	if err != nil && err.Error() != "no change" {
		log.Fatal(err)
	}

	if err != nil && err.Error() == "no change" {
		log.Println("No changes found, skipping the build")
		return
	}
	executable, err := getContainerExecutable()
	if err != nil {
		log.Fatal(err)
	}
	if *overwriteContainerExecutable != "" {
		executable = *overwriteContainerExecutable
	}
	execName := filepath.Base(executable)
	// We explicitly use /tmp, because Docker only allows volume mounts under
	// certain paths on certain platforms, see
	// e.g. https://docs.docker.com/docker-for-mac/osxfs/#namespaces for macOS.
	tmp, err := os.MkdirTemp("/tmp", "gokr-rebuild-kernel")
	if err != nil {
		log.Fatal(err)
	}
	defer os.RemoveAll(tmp)

	cmd := exec.Command("go", "install", "development.thatwebsite.xyz/gokrazy/kernel/cmd/gokr-build-kernel")
	cmd.Env = append(os.Environ(), "GOOS=linux", "GOBIN="+tmp, "CGO_ENABLED=0")
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		log.Fatalf("%v: %v", cmd.Args, err)
	}

	buildPath := filepath.Join(tmp, "gokr-build-kernel")

	var patchPaths []string
	for _, filename := range patchFiles {
		path, err := find(filename)
		if err != nil {
			log.Fatal(err)
		}
		patchPaths = append(patchPaths, path)
	}

	kernelPath, err := find("vmlinuz")
	if err != nil {
		log.Fatal(err)
	}
	dtbPath, err := find("bcm2710-rpi-3-b.dtb")
	if err != nil {
		log.Fatal(err)
	}
	dtbPlusPath, err := find("bcm2710-rpi-3-b-plus.dtb")
	if err != nil {
		log.Fatal(err)
	}
	dtbZero2WPath, err := find("bcm2710-rpi-zero-2.dtb")
	if err != nil {
		log.Fatal(err)
	}
	dtbCM3Path, err := find("bcm2710-rpi-cm3.dtb")
	if err != nil {
		log.Fatal(err)
	}
	dtb4Path, err := find("bcm2711-rpi-4-b.dtb")
	if err != nil {
		log.Fatal(err)
	}
	libPath, err := find("lib")
	if err != nil {
		log.Fatal(err)
	}

	// Copy all files into the temporary directory so that docker
	// includes them in the build context.
	for _, path := range patchPaths {
		if err := copyFile(filepath.Join(tmp, filepath.Base(path)), path); err != nil {
			log.Fatal(err)
		}
	}

	u, err := user.Current()
	if err != nil {
		log.Fatal(err)
	}
	dockerFile, err := os.Create(filepath.Join(tmp, "Dockerfile"))
	if err != nil {
		log.Fatal(err)
	}

	if err := dockerFileTmpl.Execute(dockerFile, struct {
		Uid       string
		Gid       string
		BuildPath string
		Patches   []string
	}{
		Uid:       u.Uid,
		Gid:       u.Gid,
		BuildPath: buildPath,
		Patches:   patchFiles,
	}); err != nil {
		log.Fatal(err)
	}

	if err := dockerFile.Close(); err != nil {
		log.Fatal(err)
	}

	log.Printf("building %s container for kernel compilation", execName)

	dockerBuild := exec.Command(execName,
		"build",
		"--rm=true",
		"--tag=gokr-rebuild-kernel",
		".")
	dockerBuild.Dir = tmp
	dockerBuild.Stdout = os.Stdout
	dockerBuild.Stderr = os.Stderr
	if err := dockerBuild.Run(); err != nil {
		log.Fatalf("%s build: %v (cmd: %v)", execName, err, dockerBuild.Args)
	}

	log.Printf("compiling kernel")

	var dockerRun *exec.Cmd
	if execName == "podman" {
		dockerRun = exec.Command(executable,
			"run",
			"--userns=keep-id",
			"--rm",
			"--volume", tmp+":/tmp/buildresult:Z",
			"gokr-rebuild-kernel")
	} else {
		dockerRun = exec.Command(executable,
			"run",
			"--rm",
			"--volume", tmp+":/tmp/buildresult:Z",
			"gokr-rebuild-kernel")
	}
	dockerRun.Dir = tmp
	dockerRun.Stdout = os.Stdout
	dockerRun.Stderr = os.Stderr
	if err := dockerRun.Run(); err != nil {
		log.Fatalf("%s run: %v (cmd: %v)", execName, err, dockerRun.Args)
	}

	if err := copyFile(kernelPath, filepath.Join(tmp, "vmlinuz")); err != nil {
		log.Fatal(err)
	}

	if err := copyFile(dtbPath, filepath.Join(tmp, "bcm2710-rpi-3-b.dtb")); err != nil {
		log.Fatal(err)
	}

	if err := copyFile(dtbZero2WPath, filepath.Join(tmp, "bcm2710-rpi-zero-2-w.dtb")); err != nil {
		log.Fatal(err)
	}

	if err := copyFile(dtbPlusPath, filepath.Join(tmp, "bcm2710-rpi-3-b-plus.dtb")); err != nil {
		log.Fatal(err)
	}

	if err := copyFile(dtbCM3Path, filepath.Join(tmp, "bcm2710-rpi-cm3.dtb")); err != nil {
		log.Fatal(err)
	}

	if err := copyFile(dtb4Path, filepath.Join(tmp, "bcm2711-rpi-4-b.dtb")); err != nil {
		log.Fatal(err)
	}

	// remove symlinks that only work when source/build directory are present
	for _, subdir := range []string{"build", "source"} {
		matches, err := filepath.Glob(filepath.Join(tmp, "lib/modules", "*", subdir))
		if err != nil {
			log.Fatal(err)
		}
		for _, match := range matches {
			if err := os.Remove(match); err != nil {
				log.Fatal(err)
			}
		}
	}

	// replace kernel modules directory
	rm := exec.Command("rm", "-rf", filepath.Join(libPath, "modules"))
	rm.Stdout = os.Stdout
	rm.Stderr = os.Stderr
	if err := rm.Run(); err != nil {
		log.Fatalf("%v: %v", rm.Args, err)
	}
	cp := exec.Command("cp", "-r", filepath.Join(tmp, "lib/modules"), libPath)
	cp.Stdout = os.Stdout
	cp.Stderr = os.Stderr
	if err := cp.Run(); err != nil {
		log.Fatalf("%v: %v", cp.Args, err)
	}

	if err := pushBuild(); err != nil {
		log.Fatal(err)
	}
}

func pushBuild() error {
	if err := exec.Command("git", "add", ".").Run(); err != nil {
		return err
	}

	if o, err := exec.Command("git", "commit", "-m", fmt.Sprintf("Built to version %s", latestVersion)).CombinedOutput(); err != nil {
		log.Println(string(o[:]))
		// log.Fatal(err)
		log.Println("ignoring exit code 1")
	}

	if o, err := exec.Command("git", "push", "origin", fmt.Sprintf("build-%s", latestVersion)).CombinedOutput(); err != nil {
		log.Println(string(o[:]))
		return err
	}
	return nil
}

func updateVersion() error {
	r, err := http.Get(releasesURL)
	if err != nil {
		return err
	}
	var resp ReleasesResponse
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
		return err
	}

	downloadURL := ""
	for _, release := range resp.Releases {
		if release.Version == resp.LatestStable.Version {
			downloadURL = release.Source
			break
		}
	}

	latestVersion = resp.LatestStable.Version

	// update gokr-build-kernel in development branch
	d, err := os.Create(path.Join(*buildPath, "url.go"))
	if err != nil {
		return err
	}
	defer d.Close()

	t, err := template.New("url.go").Parse(urlTemplate)
	if err != nil {
		return err
	}

	if err := t.ExecuteTemplate(d, "url.go", downloadURL); err != nil {
		return err
	}

	if o, err := exec.Command("git", "status", "--short", "cmd/gokr-build-kernel/url.go").CombinedOutput(); err != nil {
		log.Println(string(o[:]))
		return err
	} else {
		if !*dobuild && strings.TrimSpace(string(o[:])) == "" {
			return fmt.Errorf("no change")
		}
	}

	// commit it
	if err := exec.Command("git", "checkout", "development").Run(); err != nil {
		return err
	}

	if err := exec.Command("git", "add", path.Join(*buildPath, "url.go")).Run(); err != nil {
		return err
	}

	if o, err := exec.Command("git", "commit", "-m", fmt.Sprintf("Upgrade to version %s", latestVersion)).CombinedOutput(); err != nil {
		log.Println(string(o[:]))
		// log.Fatal(err)
		log.Println("ignoring exit code 1")
	}

	if o, err := exec.Command("git", "push", "origin", "development").CombinedOutput(); err != nil {
		log.Println(string(o[:]))
		return err
	}

	if o, err := exec.Command("git", "checkout", "-B", fmt.Sprintf("build-%s", latestVersion)).CombinedOutput(); err != nil {
		log.Println(string(o[:]))
		return err
	}

	if !*dobuild {
		fmt.Println("*********************************")
		fmt.Println()
		fmt.Printf("Execute `go run %s` to build the kernel\n", path.Join(path.Dir(*buildPath), "gokr-rebuild-kernel", "kernel.go"))
		fmt.Println()
		fmt.Println("*********************************")
		return nil
	}
	return nil
}

type ReleasesResponse struct {
	LatestStable struct {
		Version string `json:"version"`
	} `json:"latest_stable"`
	Releases []struct {
		Iseol    bool        `json:"iseol"`
		Version  string      `json:"version"`
		Moniker  string      `json:"moniker"`
		Source   string      `json:"source"`
		Pgp      interface{} `json:"pgp"`
		Released struct {
			Timestamp int    `json:"timestamp"`
			Isodate   string `json:"isodate"`
		} `json:"released"`
		Gitweb    string      `json:"gitweb"`
		Changelog interface{} `json:"changelog"`
		Diffview  string      `json:"diffview"`
		Patch     struct {
			Full        string `json:"full"`
			Incremental string `json:"incremental"`
		} `json:"patch"`
	} `json:"releases"`
}
