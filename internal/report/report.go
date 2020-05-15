// Copyright 2020 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

// Package report provides functions to report OPA's version information to an external service and process the response.
package report

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"

	"os"
	"strings"
	"time"

	"github.com/open-policy-agent/opa/plugins/rest"
	"github.com/open-policy-agent/opa/util"
	"github.com/open-policy-agent/opa/version"
)

const (
	defaultExternalServiceURL = "https://telemetry.openpolicyagent.org"
)

// Reporter reports the version of the running OPA instance to an external service
type Reporter struct {
	body   map[string]string
	client rest.Client
}

// DataResponse represents the data returned by the external service
type DataResponse struct {
	Latest ReleaseDetails `json:"latest,omitempty"`
}

// ReleaseDetails holds information about the latest OPA release
type ReleaseDetails struct {
	Download     string `json:"download,omitempty"`      // link to download the OPA release
	ReleaseNotes string `json:"release_notes,omitempty"` // link to the OPA release notes
}

// New returns an instance of the Reporter
func New(id string) (*Reporter, error) {
	r := Reporter{}
	r.body = map[string]string{
		"id":      id,
		"version": version.Version,
	}

	url := os.Getenv("OPA_TELEMETRY_SERVICE_URL")
	if url == "" {
		url = defaultExternalServiceURL
	}

	restConfig := []byte(fmt.Sprintf(`{
		"url": %q,
	}`, url))

	client, err := rest.New(restConfig)
	if err != nil {
		return nil, err
	}
	r.client = client

	return &r, nil
}

// SendReport sends the version report to the external service
func (r *Reporter) SendReport(ctx context.Context) (*DataResponse, error) {
	rCtx, cancel := context.WithTimeout(ctx, 1*time.Second)
	defer cancel()

	resp, err := r.client.WithJSON(r.body).Do(rCtx, "POST", "/v1/version")
	if err != nil {
		return nil, err
	}

	defer util.Close(resp)

	switch resp.StatusCode {
	case http.StatusOK:
		if resp.Body != nil {
			var result DataResponse
			err := json.NewDecoder(resp.Body).Decode(&result)
			if err != nil {
				return nil, err
			}
			return &result, nil
		}
		return nil, nil
	default:
		return nil, fmt.Errorf("server replied with HTTP %v", resp.StatusCode)
	}
}

// Pretty returns OPA update information in a human-readable format
func (dr *DataResponse) Pretty() string {
	if dr.Latest.Download == "" || dr.Latest.ReleaseNotes == "" {
		return ""
	}

	parts := strings.Split(dr.Latest.ReleaseNotes, "/")
	latest := parts[len(parts)-1]

	var buf bytes.Buffer
	fmt.Fprintln(&buf, "")
	fmt.Fprintf(&buf, "# OPA is out-of-date.\n")
	fmt.Fprintf(&buf, "# OPA version %v is now available. Current version %v\n", latest, version.Version)
	fmt.Fprintf(&buf, "\n")
	fmt.Fprintf(&buf, "# Download OPA: %v\n", dr.Latest.Download)
	fmt.Fprintf(&buf, "# Release Notes: %v\n", dr.Latest.ReleaseNotes)
	return buf.String()
}
