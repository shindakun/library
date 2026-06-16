package main

import "testing"

func TestPathMapper(t *testing.T) {
	// Host sees the data dir at /host/data; the sidecar sees the same volume at
	// /data. A path under the host root must be rewritten to the sidecar root.
	m := pathMapper("/host/data", "/data")

	if got := m("/host/data/import/x.acsm"); got != "/data/import/x.acsm" {
		t.Errorf("mapped path = %q, want /data/import/x.acsm", got)
	}
	// A path outside the host root passes through unchanged.
	if got := m("/elsewhere/y.epub"); got != "/elsewhere/y.epub" {
		t.Errorf("out-of-root path = %q, want unchanged", got)
	}
}

func TestPathMapperIdentity(t *testing.T) {
	// The compose case: both roots are /data, so mapping is a no-op.
	m := pathMapper("/data", "/data")
	if got := m("/data/import/x.acsm"); got != "/data/import/x.acsm" {
		t.Errorf("identity mapping = %q, want unchanged", got)
	}
}

func TestEnv(t *testing.T) {
	t.Setenv("LIBRARY_TEST_VAR", "value")
	if got := env("LIBRARY_TEST_VAR", "def"); got != "value" {
		t.Errorf("env with set var = %q, want value", got)
	}
	if got := env("LIBRARY_UNSET_VAR_XYZ", "def"); got != "def" {
		t.Errorf("env with unset var = %q, want def", got)
	}
}
