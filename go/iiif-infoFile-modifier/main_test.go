package main

import (
	"encoding/json"
	"os"
	"reflect"
	"testing"
)

func TestTransformInfoJSON_MatchesCorrectedFixture(t *testing.T) {
	in, err := os.ReadFile("incorrect_info.json")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	want, err := os.ReadFile("corrected_info.json")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	got, err := transformInfoJSON(in)
	if err != nil {
		t.Fatalf("transformInfoJSON: %v", err)
	}

	var gotVal, wantVal interface{}
	if err := json.Unmarshal(got, &gotVal); err != nil {
		t.Fatalf("unmarshaling transform output: %v", err)
	}
	if err := json.Unmarshal(want, &wantVal); err != nil {
		t.Fatalf("unmarshaling corrected fixture: %v", err)
	}

	if !reflect.DeepEqual(gotVal, wantVal) {
		t.Fatalf("transform output does not match corrected fixture\ngot:  %s\nwant: %s", got, want)
	}
}

func TestIsPreTransformShape(t *testing.T) {
	original, err := os.ReadFile("incorrect_info.json")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	corrected, err := os.ReadFile("corrected_info.json")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	if got, err := isPreTransformShape(original); err != nil {
		t.Fatalf("isPreTransformShape(original): %v", err)
	} else if !got {
		t.Errorf("isPreTransformShape(original) = false, want true")
	}

	if got, err := isPreTransformShape(corrected); err != nil {
		t.Fatalf("isPreTransformShape(corrected): %v", err)
	} else if got {
		t.Errorf("isPreTransformShape(corrected) = true, want false")
	}
}

func TestIsAlreadyTransformed(t *testing.T) {
	original, err := os.ReadFile("incorrect_info.json")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	corrected, err := os.ReadFile("corrected_info.json")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	if got, err := isAlreadyTransformed(original); err != nil {
		t.Fatalf("isAlreadyTransformed(original): %v", err)
	} else if got {
		t.Errorf("isAlreadyTransformed(original) = true, want false")
	}

	if got, err := isAlreadyTransformed(corrected); err != nil {
		t.Fatalf("isAlreadyTransformed(corrected): %v", err)
	} else if !got {
		t.Errorf("isAlreadyTransformed(corrected) = false, want true")
	}
}

func TestLooksLikeIIIFInfoJSON(t *testing.T) {
	original, err := os.ReadFile("incorrect_info.json")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}
	corrected, err := os.ReadFile("corrected_info.json")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	if !looksLikeIIIFInfoJSON(original) {
		t.Errorf("looksLikeIIIFInfoJSON(original) = false, want true")
	}
	if !looksLikeIIIFInfoJSON(corrected) {
		t.Errorf("looksLikeIIIFInfoJSON(corrected) = false, want true")
	}

	nonIIIF := []byte(`{"format": "mp4", "duration_seconds": 120, "width": 1920, "height": 1080}`)
	if looksLikeIIIFInfoJSON(nonIIIF) {
		t.Errorf("looksLikeIIIFInfoJSON(nonIIIF) = true, want false")
	}

	if looksLikeIIIFInfoJSON([]byte(`not json`)) {
		t.Errorf("looksLikeIIIFInfoJSON(invalid JSON) = true, want false")
	}
}

func TestTransformInfoJSON_Idempotent(t *testing.T) {
	in, err := os.ReadFile("corrected_info.json")
	if err != nil {
		t.Fatalf("reading fixture: %v", err)
	}

	got, err := transformInfoJSON(in)
	if err != nil {
		t.Fatalf("transformInfoJSON: %v", err)
	}

	var gotVal, wantVal interface{}
	if err := json.Unmarshal(got, &gotVal); err != nil {
		t.Fatalf("unmarshaling transform output: %v", err)
	}
	if err := json.Unmarshal(in, &wantVal); err != nil {
		t.Fatalf("unmarshaling input fixture: %v", err)
	}

	if !reflect.DeepEqual(gotVal, wantVal) {
		t.Fatalf("transform is not idempotent on already-corrected input\ngot:  %s\nwant: %s", got, in)
	}
}
