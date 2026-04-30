package commands

import (
	"fmt"
	"io"
)

const (
	AppName = "freecode"
	Version = "0.1.0-dev"
)

type VersionInfo struct {
	Name    string
	Version string
}

func DefaultVersionInfo() VersionInfo {
	return VersionInfo{Name: AppName, Version: Version}
}

func PrintVersion(w io.Writer, info VersionInfo) error {
	info = withVersionDefaults(info)
	_, err := fmt.Fprintf(w, "%s %s\n", info.Name, info.Version)
	return err
}

func withVersionDefaults(info VersionInfo) VersionInfo {
	if info.Name == "" {
		info.Name = AppName
	}
	if info.Version == "" {
		info.Version = Version
	}
	return info
}
