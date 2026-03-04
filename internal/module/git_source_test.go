/*
Copyright 2026 The OtterScale Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package module

import (
	"testing"

	modulev1alpha1 "github.com/otterscale/api/module/v1alpha1"
)

func TestResolveGitReference_Nil(t *testing.T) {
	if got := resolveGitReference(nil); got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestResolveGitReference_Tag(t *testing.T) {
	ref := &modulev1alpha1.GitReference{Tag: "v1.0.0"}
	if got := resolveGitReference(ref); got != "refs/tags/v1.0.0" {
		t.Errorf("got %q, want %q", got, "refs/tags/v1.0.0")
	}
}

func TestResolveGitReference_Branch(t *testing.T) {
	ref := &modulev1alpha1.GitReference{Branch: "main"}
	if got := resolveGitReference(ref); got != "refs/heads/main" {
		t.Errorf("got %q, want %q", got, "refs/heads/main")
	}
}

func TestResolveGitReference_TagPrecedence(t *testing.T) {
	ref := &modulev1alpha1.GitReference{Tag: "v1.0.0", Branch: "main"}
	if got := resolveGitReference(ref); got != "refs/tags/v1.0.0" {
		t.Errorf("tag should take precedence over branch: got %q", got)
	}
}

func TestResolveGitReference_CommitOnly(t *testing.T) {
	ref := &modulev1alpha1.GitReference{Commit: "abc123"}
	if got := resolveGitReference(ref); got != "" {
		t.Errorf("commit-only should return empty (clone default branch first): got %q", got)
	}
}

func TestResolveGitReference_SemverOnly(t *testing.T) {
	ref := &modulev1alpha1.GitReference{Semver: ">=1.0.0"}
	if got := resolveGitReference(ref); got != "" {
		t.Errorf("semver-only should return empty (clone all tags first): got %q", got)
	}
}

func TestResolveGitReference_Empty(t *testing.T) {
	ref := &modulev1alpha1.GitReference{}
	if got := resolveGitReference(ref); got != "" {
		t.Errorf("empty ref should return empty: got %q", got)
	}
}

func TestSourceFetchError(t *testing.T) {
	inner := &ClassNotFoundError{Name: "x"}
	err := &SourceFetchError{URL: "https://example.com/repo.git", Err: inner}

	want := `failed to fetch source "https://example.com/repo.git": moduleclass "x" not found`
	if err.Error() != want {
		t.Errorf("got %q, want %q", err.Error(), want)
	}
	if err.Unwrap() != inner {
		t.Error("Unwrap should return inner error")
	}
}

func TestChartFetchError(t *testing.T) {
	inner := &ClassNotFoundError{Name: "y"}
	err := &ChartFetchError{Chart: "nginx", RepoURL: "https://charts.example.com", Err: inner}

	want := `failed to fetch chart "nginx" from "https://charts.example.com": moduleclass "y" not found`
	if err.Error() != want {
		t.Errorf("got %q, want %q", err.Error(), want)
	}
	if err.Unwrap() != inner {
		t.Error("Unwrap should return inner error")
	}
}
