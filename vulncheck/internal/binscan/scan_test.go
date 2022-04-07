// Copyright 2021 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.18
// +build go1.18

package binscan

import (
	"os"
	"testing"

	"github.com/google/go-cmp/cmp"
	"golang.org/x/vuln/vulncheck/internal/buildtest"
)

func TestExtractPackagesAndSymbols(t *testing.T) {
	binary, done := buildtest.GoBuild(t, "testdata")
	defer done()

	f, err := os.Open(binary)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	_, syms, err := ExtractPackagesAndSymbols(f)
	if err != nil {
		t.Fatal(err)
	}
	got := syms["main"]
	want := []string{"main"}
	if !cmp.Equal(got, want) {
		t.Errorf("\ngot  %q\nwant %q", got, want)
	}
}