package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path"
	"text/template"
)

const (
	releasesURL = "https://www.kernel.org/releases.json"
)

var (
	buildPath   = flag.String("build-path", "cmd/gokr-build-kernel", "Build Package path")
	urlTemplate = `
package main

// see https://www.kernel.org/releases.json
var latest = "{{ . }}"
`
)

func main() {
	flag.Parse()
	log.SetFlags(log.LstdFlags | log.Lshortfile)
	// get latest stable release
	r, err := http.Get(releasesURL)
	if err != nil {
		log.Fatal(err)
	}
	var resp ReleasesResponse
	defer r.Body.Close()
	if err := json.NewDecoder(r.Body).Decode(&resp); err != nil {
		log.Fatal(err)
	}

	downloadURL := ""
	for _, release := range resp.Releases {
		if release.Version == resp.LatestStable.Version {
			downloadURL = release.Source
			break
		}
	}
	// update gokr-build-kernel in development branch

	d, err := os.Create(path.Join(*buildPath, "url.go"))
	if err != nil {
		log.Fatal(err)
	}
	defer d.Close()

	t, err := template.New("url.go").Parse(urlTemplate)
	if err != nil {
		log.Fatal(err)
	}

	if err := t.ExecuteTemplate(d, "url.go", downloadURL); err != nil {
		log.Fatal(err)
	}

	// commit it
	if err := exec.Command("git", "checkout", "development").Run(); err != nil {
		log.Fatal(err)
	}

	if err := exec.Command("git", "add", path.Join(*buildPath, "url.go")).Run(); err != nil {
		log.Fatal(err)
	}

	if o, err := exec.Command("git", "commit", "-m", fmt.Sprintf("Upgrade to version %s", resp.LatestStable.Version)).CombinedOutput(); err != nil {
		log.Println(string(o[:]))
		log.Fatal(err)
	}

	if o, err := exec.Command("git", "checkout", "-B", fmt.Sprintf("build-%s", resp.LatestStable.Version)).CombinedOutput(); err != nil {
		log.Println(string(o[:]))
		log.Fatal(err)
	}
	fmt.Println("*********************************")
	fmt.Println()
	fmt.Printf("Execute `go run %s` to build the kernel\n", path.Join(path.Dir(*buildPath), "gokr-rebuild-kernel", "kernel.go"))
	fmt.Println()
	fmt.Println("*********************************")

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
