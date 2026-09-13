package main

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

const namespace = "local-notifier"

type artifact struct {
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	Path   string `json:"path"`
	Format string `json:"format"`
	Entry  string `json:"entry"`
	SHA256 string `json:"sha256"`
}

type manifest struct {
	Version       int        `json:"version"`
	Namespace     string     `json:"namespace"`
	PluginVersion string     `json:"plugin_version"`
	Protocol      string     `json:"protocol"`
	Artifacts     []artifact `json:"artifacts"`
}

type target struct {
	os    string
	arch  string
	label string
}

func main() {
	version := flag.String("version", "", "plugin version")
	flag.Parse()
	if strings.TrimSpace(*version) == "" || strings.TrimSpace(*version) != *version {
		fatal(fmt.Errorf("version must be non-empty without surrounding whitespace"))
	}
	cwd, err := os.Getwd()
	if err != nil {
		fatal(err)
	}
	stage, err := os.MkdirTemp(cwd, ".release-*")
	if err != nil {
		fatal(err)
	}
	defer os.RemoveAll(stage)

	targets := []target{
		{os: "darwin", arch: "amd64", label: "Darwin"},
		{os: "darwin", arch: "arm64", label: "Darwin"},
		{os: "linux", arch: "amd64", label: "Linux"},
		{os: "linux", arch: "arm64", label: "Linux"},
	}
	result := manifest{Version: 1, Namespace: namespace, PluginVersion: *version, Protocol: "wuko.plugin/v1"}
	for _, item := range targets {
		binary := filepath.Join(stage, "bin", item.os+"-"+item.arch, "wuko-plugin-"+namespace)
		if err := os.MkdirAll(filepath.Dir(binary), 0o755); err != nil {
			fatal(err)
		}
		command := exec.Command("go", "build", "-trimpath", "-buildvcs=false", "-ldflags=-s -w", "-o", binary, ".")
		command.Env = append(os.Environ(), "CGO_ENABLED=0", "GOOS="+item.os, "GOARCH="+item.arch)
		command.Stdout = os.Stdout
		command.Stderr = os.Stderr
		if err := command.Run(); err != nil {
			fatal(fmt.Errorf("building %s/%s: %w", item.os, item.arch, err))
		}

		name := "wuko-plugin-" + namespace + "_" + item.label + "_" + item.arch + ".tar.gz"
		relative := filepath.ToSlash(filepath.Join("dist", name))
		archivePath := filepath.Join(stage, filepath.FromSlash(relative))
		if err := os.MkdirAll(filepath.Dir(archivePath), 0o755); err != nil {
			fatal(err)
		}
		if err := writeArchive(binary, archivePath); err != nil {
			fatal(err)
		}
		data, err := os.ReadFile(archivePath)
		if err != nil {
			fatal(err)
		}
		sum := sha256.Sum256(data)
		result.Artifacts = append(result.Artifacts, artifact{
			OS: item.os, Arch: item.arch, Path: relative, Format: "tar.gz",
			Entry: "wuko-plugin-" + namespace, SHA256: hex.EncodeToString(sum[:]),
		})
	}

	manifestPath := filepath.Join(stage, "plugin.json")
	file, err := os.Create(manifestPath)
	if err != nil {
		fatal(err)
	}
	encoder := json.NewEncoder(file)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(result); err != nil {
		file.Close()
		fatal(err)
	}
	if err := file.Close(); err != nil {
		fatal(err)
	}
	if err := replace(filepath.Join(cwd, "dist"), filepath.Join(stage, "dist")); err != nil {
		fatal(err)
	}
	if err := replace(filepath.Join(cwd, "plugin.json"), manifestPath); err != nil {
		fatal(err)
	}
	fmt.Printf("released plugin %s %s for %d platforms\n", namespace, *version, len(targets))
}

func writeArchive(binary, destination string) error {
	input, err := os.Open(binary)
	if err != nil {
		return err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return err
	}
	output, err := os.Create(destination)
	if err != nil {
		return err
	}
	gzipWriter := gzip.NewWriter(output)
	gzipWriter.Header.ModTime = time.Unix(0, 0)
	gzipWriter.Header.OS = 255
	tarWriter := tar.NewWriter(gzipWriter)
	header := &tar.Header{
		Name: "wuko-plugin-" + namespace, Mode: 0o755, Size: info.Size(),
		ModTime: time.Unix(0, 0), AccessTime: time.Unix(0, 0), ChangeTime: time.Unix(0, 0),
		Format: tar.FormatPAX,
	}
	if err := tarWriter.WriteHeader(header); err != nil {
		output.Close()
		return err
	}
	if _, err := io.Copy(tarWriter, input); err != nil {
		output.Close()
		return err
	}
	if err := tarWriter.Close(); err != nil {
		output.Close()
		return err
	}
	if err := gzipWriter.Close(); err != nil {
		output.Close()
		return err
	}
	return output.Close()
}

func replace(target, staged string) error {
	if err := os.RemoveAll(target); err != nil {
		return err
	}
	return os.Rename(staged, target)
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
