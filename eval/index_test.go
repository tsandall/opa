// Copyright 2016 The OPA Authors.  All rights reserved.
// Use of this source code is governed by an Apache2
// license that can be found in the LICENSE file.

package eval

import (
	"fmt"
	"reflect"
	"testing"
)

func TestIndicesBuild(t *testing.T) {

	tests := []struct {
		note     string
		ref      string
		value    interface{}
		expected string
	}{
		{"single var", "a[i]", float64(2), `[{"i": 1}]`},
		{"two var", "d[x][y]", "baz", `[{"x": "e", "y": 1}]`},
		{"partial ground", `c[i]["y"][j]`, nil, `[{"i": 0, "j": 0}]`},
		{"multiple bindings", "g[x][y]", float64(0), `[
			{"x": "a", "y": 1},
			{"x": "a", "y": 2},
			{"x": "a", "y": 3},
			{"x": "b", "y": 0},
			{"x": "b", "y": 2},
			{"x": "b", "y": 3},
			{"x": "c", "y": 0},
			{"x": "c", "y": 1},
			{"x": "c", "y": 2}
		]`},
	}

	for i, tc := range tests {
		runIndexBuildTestCase(t, i+1, tc.note, tc.ref, tc.expected, tc.value)
	}

}

func runIndexBuildTestCase(t *testing.T, i int, note string, refStr string, expectedStr string, value interface{}) {

	indices := NewIndices()
	data := loadSmallTestData()
	store := NewStorageFromJSONObject(data)
	ref := parseRef(refStr)

	if indices.Contains(ref) {
		t.Errorf("Test case %d (%v): Did not expect indices to contain %v yet", i, note, ref)
		return
	}

	err := indices.Build(store, ref)
	if err != nil {
		t.Errorf("Test case %d (%v): Did not expect error from build: %v", i, note, err)
		return
	}

	index := indices.Get(ref)
	if index == nil {
		t.Errorf("Test case %d (%v): Did not expect nil index for %v", i, note, ref)
		return
	}

	expected := loadExpectedBindings(expectedStr)

	err = index.Iter(value, func(bindings *hashMap) error {
		for j := range expected {
			if reflect.DeepEqual(expected[j], bindings) {
				tmp := expected[:j]
				expected = append(tmp, expected[j+1:]...)
				return nil
			}
		}
		return fmt.Errorf("unexpected bindings: %v", bindings)
	})

	if err != nil {
		t.Errorf("Test case %d (%v): Did not expect error from index iteration: %v", i, note, err)
		return
	}

	if len(expected) > 0 {
		t.Errorf("Test case %d (%v): Missing expected bindings: %v", i, note, expected)
		return
	}

}
