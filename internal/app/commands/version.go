package commands

import (
	"fmt"
	"io"
)

const (
	AppName = "freecode"
)

var (
	Version = "0.1.0-dev"
	Commit  = "unknown"
	Date    = "unknown"
)

type VersionInfo struct {
	Name    string
	Version string
	Commit  string
	Date    string
}

func DefaultVersionInfo() VersionInfo {
	return VersionInfo{Name: AppName, Version: Version, Commit: Commit, Date: Date}
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
	if info.Commit == "" {
		info.Commit = Commit
	}
	if info.Date == "" {
		info.Date = Date
	}
	return info
}
