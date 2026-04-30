package commands

import (
	"fmt"
	"io"
)

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
	Version VersionInfo
	WorkDir string
	Runtime RuntimeStatus
	Checks  []DoctorCheck
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
