// Copyright 2020 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

package report

import (
	"context"
	"encoding/json"

	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"testing"

	"github.com/open-policy-agent/opa/version"
)

func TestNewReportDefaultURL(t *testing.T) {
	os.Unsetenv("OPA_TELEMETRY_SERVICE_URL")

	reporter, err := New("")
	if err != nil {
		t.Fatalf("Unexpected error %v", err)
	}

	actual := reporter.client.Config().URL
	if actual != defaultExternalServiceURL {
		t.Fatalf("Expected server URL %v but got %v", defaultExternalServiceURL, actual)
	}
}

func TestSendReportBadRespStatus(t *testing.T) {

	// test server
	baseURL, teardown := getTestServer(nil, http.StatusBadRequest)
	defer teardown()

	os.Setenv("OPA_TELEMETRY_SERVICE_URL", baseURL)

	reporter, err := New("")
	if err != nil {
		t.Fatalf("Unexpected error %v", err)
	}

	_, err = reporter.SendReport(context.Background())

	if err == nil {
		t.Fatal("Expected error but got nil")
	}

	expectedErrMsg := "server replied with HTTP 400"
	if expectedErrMsg != err.Error() {
		t.Fatalf("Expected error: %v but got: %v", expectedErrMsg, err.Error())
	}
}

func TestSendReportDecodeError(t *testing.T) {

	// test server
	baseURL, teardown := getTestServer("foo", http.StatusOK)
	defer teardown()

	os.Setenv("OPA_TELEMETRY_SERVICE_URL", baseURL)

	reporter, err := New("")
	if err != nil {
		t.Fatalf("Unexpected error %v", err)
	}

	_, err = reporter.SendReport(context.Background())

	if err == nil {
		t.Fatal("Expected error but got nil")
	}
}

func TestSendReportNoOPAUpdate(t *testing.T) {

	// test server
	baseURL, teardown := getTestServer(nil, http.StatusOK)
	defer teardown()

	os.Setenv("OPA_TELEMETRY_SERVICE_URL", baseURL)

	reporter, err := New("")
	if err != nil {
		t.Fatalf("Unexpected error %v", err)
	}

	resp, err := reporter.SendReport(context.Background())

	if err != nil {
		t.Fatalf("Expected no error but got %v", err)
	}

	if resp.Latest.Download != "" {
		t.Fatalf("Expected empty downlaod link but got %v", resp.Latest.Download)
	}

	if resp.Latest.ReleaseNotes != "" {
		t.Fatalf("Expected empty release notes but got %v", resp.Latest.ReleaseNotes)
	}
}

func TestSendReportWithOPAUpdate(t *testing.T) {
	exp := &DataResponse{Latest: ReleaseDetails{
		Download:     "https://openpolicyagent.org/downloads/v100.0.0/opa_darwin_amd64",
		ReleaseNotes: "https://github.com/open-policy-agent/opa/releases/tag/v100.0.0",
	}}

	// test server
	baseURL, teardown := getTestServer(exp, http.StatusOK)
	defer teardown()

	os.Setenv("OPA_TELEMETRY_SERVICE_URL", baseURL)

	reporter, err := New("")
	if err != nil {
		t.Fatalf("Unexpected error %v", err)
	}

	resp, err := reporter.SendReport(context.Background())

	if err != nil {
		t.Fatalf("Expected no error but got %v", err)
	}

	if !reflect.DeepEqual(resp, exp) {
		t.Fatalf("Expected response: %+v but got: %+v", exp, resp)
	}
}

func TestPretty(t *testing.T) {
	dr := DataResponse{}
	resp := dr.Pretty()

	if resp != "" {
		t.Fatalf("Expected empty response but got %v", resp)
	}

	dr.Latest.Download = "https://openpolicyagent.org/downloads/v100.0.0/opa_darwin_amd64"
	resp = dr.Pretty()

	if resp != "" {
		t.Fatalf("Expected empty response but got %v", resp)
	}

	dr.Latest.ReleaseNotes = "https://github.com/open-policy-agent/opa/releases/tag/v100.0.0"
	version.Version = "v0.20.0"
	resp = dr.Pretty()

	exp := "\n# OPA is out-of-date.\n" +
		"# OPA version v100.0.0 is now available. Current version v0.20.0\n" + "\n" +
		"# Download OPA: https://openpolicyagent.org/downloads/v100.0.0/opa_darwin_amd64\n" +
		"# Release Notes: https://github.com/open-policy-agent/opa/releases/tag/v100.0.0\n"

	if resp != exp {
		t.Fatalf("Expected response:%v but got %v", exp, resp)
	}
}

func getTestServer(update interface{}, statusCode int) (baseURL string, teardownFn func()) {
	mux := http.NewServeMux()
	ts := httptest.NewServer(mux)

	mux.HandleFunc("/v1/version", func(w http.ResponseWriter, req *http.Request) {
		w.WriteHeader(statusCode)
		bs, _ := json.Marshal(update)
		w.Header().Set("Content-Type", "application/json")
		w.Write(bs)
	})
	return ts.URL, ts.Close
}
