package commands

import (
	"fmt"
	"io"
)

type DoctorOptions struct {
	ConfigPath string
	WorkDir    string
}

type RuntimeStatus struct {
	GoVersion string
	GOOS      string
	GOARCH    string
}

type DoctorCheck struct {
	Name   string
	OK     bool
	Detail string
}

type DoctorStatus struct {
	Version     VersionInfo
	WorkDir     string
	ConfigPath  string
	ActiveModel string
	Approval    string
	Runtime     RuntimeStatus
	Checks      []DoctorCheck
}

func PrintDoctor(w io.Writer, status DoctorStatus) error {
	version := withVersionDefaults(status.Version)
	if _, err := fmt.Fprintln(w, "FreeCode doctor"); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "name: %s\n", version.Name); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "version: %s\n", version.Version); err != nil {
		return err
	}
	if version.Commit != "" && version.Commit != "unknown" {
		if _, err := fmt.Fprintf(w, "commit: %s\n", version.Commit); err != nil {
			return err
		}
	}
	if status.ConfigPath != "" {
		if _, err := fmt.Fprintf(w, "config: %s\n", status.ConfigPath); err != nil {
			return err
		}
	}
	if status.ActiveModel != "" {
		if _, err := fmt.Fprintf(w, "active_model: %s\n", status.ActiveModel); err != nil {
			return err
		}
	}
	if status.Approval != "" {
		if _, err := fmt.Fprintf(w, "approval: %s\n", status.Approval); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "go: %s %s/%s\n", status.Runtime.GoVersion, status.Runtime.GOOS, status.Runtime.GOARCH); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(w, "cwd: %s\n", status.WorkDir); err != nil {
		return err
	}
	if _, err := fmt.Fprintln(w, "checks:"); err != nil {
		return err
	}
	for _, check := range status.Checks {
		label := "warn"
		if check.OK {
			label = "ok"
		}
		if _, err := fmt.Fprintf(w, "  %-4s %s: %s\n", label, check.Name, check.Detail); err != nil {
			return err
		}
	}
	return nil
}
