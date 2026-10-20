// Copyright The Go Authors. All rights reserved. Use of this source code is
// governed by a BSD-style license that can be found in the LICENSE file.

package packages

// This file defines the protocol that enables an external "driver"
// tool to supply package metadata in place of 'go list'.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"
)

// DriverRequest defines the schema of a request for package metadata from an
// external driver program. The JSON-encoded DriverRequest message is provided
// to the driver program's standard input.
type DriverRequest struct {
	Mode LoadMode `json:"mode"`

	// Env specifies the environment the underlying build system should be run in.
	Env []string `json:"env"`

	// BuildFlags are flags that should be passed to the underlying build system.
	BuildFlags []string `json:"build_flags"`

	// Tests specifies whether the patterns should also return test packages.
	Tests bool `json:"tests"`

	// Overlay maps file paths (relative to the driver's working directory) to the contents of overlay files.
	Overlay map[string][]byte `json:"overlay"`
}

// DriverResponse defines the schema of a response from an external driver
// program, providing the results of a query for package metadata.
type DriverResponse struct {
	// NotHandled is returned if the request can't be handled by the current driver.
	NotHandled bool

	// Compiler and Arch are the arguments pass of types.SizesFor to get a types.Sizes to use when type checking.
	Compiler string
	Arch     string

	// Roots is the set of package IDs that make up the root packages.
	Roots []string `json:",omitempty"`

	// Packages is the full set of packages in the graph. The packages are not connected into a graph.
	Packages []*Package

	// GoVersion is the minor version number used by the driver (e.g. the go command on the PATH) when selecting .go files.
	GoVersion int
}

// driver is the type for functions that query the build system for the packages named by the patterns.
type driver func(cfg *Config, patterns []string) (*DriverResponse, error)

// findExternalDriver returns the file path of a tool that supplies the build
// system package structure, or "" if not found.
func findExternalDriver(cfg *Config) driver {
	const toolPrefix = "GOPACKAGESDRIVER="
	tool := ""
	for _, env := range cfg.Env {
		if val, ok := strings.CutPrefix(env, toolPrefix); ok {
			tool = val
		}
	}
	if tool != "" && tool == "off" {
		return nil
	}
	if tool == "" {
		var err error
		tool, err = exec.LookPath("gopackagesdriver")
		if err != nil {
			return nil
		}
	}
	return func(cfg *Config, patterns []string) (*DriverResponse, error) {
		req, err := json.Marshal(DriverRequest{
			Mode:       cfg.Mode,
			Env:        cfg.Env,
			BuildFlags: cfg.BuildFlags,
			Tests:      cfg.Tests,
			Overlay:    cfg.Overlay,
		})
		if err != nil {
			return nil, fmt.Errorf("failed to encode message to driver tool: %v", err)
		}

		buf := new(bytes.Buffer)
		stderr := new(bytes.Buffer)
		cmd := exec.CommandContext(cfg.Context, tool, patterns...)
		cmd.Dir = cfg.Dir
		// The cwd gets resolved to the real path.
		cmd.Env = append(slices.Clip(cfg.Env), "PWD="+cfg.Dir)
		cmd.Stdin = bytes.NewReader(req)
		cmd.Stdout = buf
		cmd.Stderr = stderr

		if err := cmd.Run(); err != nil {
			return nil, fmt.Errorf("%v: %v: %s", tool, err, cmd.Stderr)
		}
		if len(stderr.Bytes()) != 0 && os.Getenv("GOPACKAGESPRINTDRIVERERRORS") != "" {
			fmt.Fprintf(os.Stderr, "%s stderr: <<%s>>\n", cmdDebugStr(cmd), stderr)
		}

		var response DriverResponse
		if err := json.Unmarshal(buf.Bytes(), &response); err != nil {
			return nil, err
		}
		return &response, nil
	}
}
