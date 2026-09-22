package main

import (
	"reflect"
	"testing"
)

func strp(s string) *string { return &s }

func TestPlanApply_FiltersByCollectionAndFormatsTitle(t *testing.T) {
	rep := &Report{
		Duplicates: []Group{
			{
				Title: "1957 Coeburn Quadrangle Virginia",
				Records: []Record{
					{Id: "id-1", Identifier: "nmcst005196", ParentCollection: strp("coll-a")},
					{Id: "id-2", Identifier: "nmcst005197", ParentCollection: strp("coll-a")},
					{Id: "id-3", Identifier: "other0001", ParentCollection: strp("coll-b")},
				},
			},
		},
	}

	jobs := planApply(rep, "coll-a", "Map")

	want := []Job{
		{Id: "id-1", Identifier: "nmcst005196", OldTitle: "1957 Coeburn Quadrangle Virginia", NewTitle: "1957 Coeburn Quadrangle Virginia - Map:nmcst005196"},
		{Id: "id-2", Identifier: "nmcst005197", OldTitle: "1957 Coeburn Quadrangle Virginia", NewTitle: "1957 Coeburn Quadrangle Virginia - Map:nmcst005197"},
	}
	if !reflect.DeepEqual(jobs, want) {
		t.Fatalf("planApply() = %+v, want %+v", jobs, want)
	}
}

func TestPlanApply_MixedCollectionGroupOnlyIncludesMatchingRecords(t *testing.T) {
	rep := &Report{
		Duplicates: []Group{
			{
				Title: "American Flag",
				Records: []Record{
					{Id: "id-1", Identifier: "fchs_2012_034_001", ParentCollection: strp("coll-a")},
					{Id: "id-2", Identifier: "sfdst006019", ParentCollection: strp("coll-b")},
				},
			},
		},
	}

	jobsA := planApply(rep, "coll-a", "Photo")
	if len(jobsA) != 1 || jobsA[0].Identifier != "fchs_2012_034_001" {
		t.Fatalf("planApply(coll-a) = %+v, want exactly the coll-a record", jobsA)
	}

	jobsB := planApply(rep, "coll-b", "Photo")
	if len(jobsB) != 1 || jobsB[0].Identifier != "sfdst006019" {
		t.Fatalf("planApply(coll-b) = %+v, want exactly the coll-b record", jobsB)
	}
}

func TestPlanApply_SkipsRecordsWithNoParentCollection(t *testing.T) {
	rep := &Report{
		Duplicates: []Group{
			{
				Title: "Untitled",
				Records: []Record{
					{Id: "id-1", Identifier: "no-parent", ParentCollection: nil},
					{Id: "id-2", Identifier: "has-parent", ParentCollection: strp("coll-a")},
				},
			},
		},
	}

	jobs := planApply(rep, "coll-a", "Item")
	if len(jobs) != 1 || jobs[0].Identifier != "has-parent" {
		t.Fatalf("planApply() = %+v, want only the record with a matching parent_collection", jobs)
	}
}

func TestPlanApply_IsIdempotent(t *testing.T) {
	rep := &Report{
		Duplicates: []Group{
			{
				Title: "1957 Coeburn Quadrangle Virginia",
				Records: []Record{
					{Id: "id-1", Identifier: "nmcst005196", ParentCollection: strp("coll-a")},
				},
			},
		},
	}

	first := planApply(rep, "coll-a", "Map")
	second := planApply(rep, "coll-a", "Map")
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("planApply() is not idempotent: %+v != %+v", first, second)
	}
}

func TestPlanRollback_RestoresRecordedOriginalRegardlessOfCollection(t *testing.T) {
	newTitleA := "1957 Coeburn Quadrangle Virginia - Map:nmcst005196"
	newTitleB := "American Flag - Photo:sfdst006019"
	rep := &Report{
		CollectionIdentifier: "coll-a",
		Timestamp:            "20260922T000000Z",
		Duplicates: []Group{
			{
				Title: "1957 Coeburn Quadrangle Virginia",
				Records: []Record{
					{Id: "id-1", Identifier: "nmcst005196", ParentCollection: strp("coll-a"), NewTitle: &newTitleA},
				},
			},
			{
				Title: "American Flag",
				Records: []Record{
					{Id: "id-2", Identifier: "sfdst006019", ParentCollection: strp("coll-b"), NewTitle: &newTitleB},
				},
			},
		},
	}

	jobs := planRollback(rep)

	want := []Job{
		{Id: "id-1", Identifier: "nmcst005196", OldTitle: newTitleA, NewTitle: "1957 Coeburn Quadrangle Virginia"},
		{Id: "id-2", Identifier: "sfdst006019", OldTitle: newTitleB, NewTitle: "American Flag"},
	}
	if !reflect.DeepEqual(jobs, want) {
		t.Fatalf("planRollback() = %+v, want %+v", jobs, want)
	}
}

func TestPlanRollback_WorksOnAPlainReportWithNoNewTitle(t *testing.T) {
	rep := &Report{
		Duplicates: []Group{
			{
				Title: "1957 Coeburn Quadrangle Virginia",
				Records: []Record{
					{Id: "id-1", Identifier: "nmcst005196", ParentCollection: strp("coll-a")},
				},
			},
		},
	}

	jobs := planRollback(rep)
	want := []Job{
		{Id: "id-1", Identifier: "nmcst005196", OldTitle: "", NewTitle: "1957 Coeburn Quadrangle Virginia"},
	}
	if !reflect.DeepEqual(jobs, want) {
		t.Fatalf("planRollback() = %+v, want %+v", jobs, want)
	}
}
