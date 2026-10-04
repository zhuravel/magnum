package engine

import (
	"slices"
	"testing"

	"github.com/zhuravel/magnum/internal/github"
)

// modifiedFile is a modified file of a delta with its patch.
func modifiedFile(p, patch string) github.FileDelta {
	return github.FileDelta{Path: p, Status: "modified", Patch: patch}
}

// The push that motivated skip_trivial_deltas: only "# sizing" comments of a
// compose file changed, over two hunks.
func TestTrivialDeltaLiveYAMLComments(t *testing.T) {
	files := []github.FileDelta{modifiedFile("deploy/compose.yml", `@@ -12,10 +12,10 @@ services:
   worker:
     image: example/worker:latest
-    # sizing: 2 CPUs and 4 GB were enough for the old queue
-    # sizing: revisit after the March load test
+    # sizing: 2 CPUs and 4 GB cover the staging queue; production runs 4
+    # sizing: measured in the September load test
     resources:
       cpus: 2
       memory: 4g
@@ -40,6 +40,7 @@ services:
   scheduler:
     image: example/scheduler:latest
+    # sizing: one CPU is plenty, it only enqueues
     resources:
       cpus: 1
       memory: 1g`)}
	classes, ok := TrivialDelta(files, DeltaClasses)
	if !ok || !slices.Equal(classes, []string{DeltaComments}) {
		t.Fatalf("TrivialDelta = %v, %v; want [comments], true", classes, ok)
	}
	if got := DeltaLabel(classes); got != "comments only" {
		t.Fatalf("DeltaLabel = %q, want %q", got, "comments only")
	}
}

func TestTrivialDelta(t *testing.T) {
	goComment := modifiedFile("internal/example/limit.go", `@@ -1,3 +1,4 @@
 package example
+// Limit caps the batch.
 const Limit = 10`)
	rubyWhitespace := modifiedFile("app/models/example.rb", `@@ -1,4 +1,4 @@
 def call
-  run(1)  
-    finish
+  run(1)
+  finish
 end`)
	readme := modifiedFile("README.md", `@@ -1,3 +1,3 @@
 # Example
-Run make.
+Run make test.
 More text.`)
	docsYAML := modifiedFile("docs/guide/setup.yml", `@@ -1,2 +1,2 @@
 server:
-  port: 1
+  port: 2`)
	rubyCommentAndBlank := modifiedFile("app/models/example.rb", `@@ -1,3 +1,5 @@
 def call
+  # Runs once per batch.
+
   run
 end`)
	withStatus := func(f github.FileDelta, status string) github.FileDelta {
		f.Status = status
		return f
	}

	cases := []struct {
		name    string
		files   []github.FileDelta
		allowed []string // nil: every class
		want    []string // nil: not trivial
	}{
		{name: "ruby comment and code", files: []github.FileDelta{modifiedFile("app/models/example.rb", `@@ -1,4 +1,4 @@
 class Example
-  # old note
-  LIMIT = 10
+  # new note
+  LIMIT = 20
 end`)}},
		{name: "docs only", files: []github.FileDelta{readme, docsYAML}, want: []string{DeltaDocs}},
		{name: "docs not allowed", files: []github.FileDelta{readme, docsYAML}, allowed: []string{DeltaComments, DeltaWhitespace}},
		{name: "docs not allowed, markdown whitespace", allowed: []string{DeltaWhitespace},
			files: []github.FileDelta{modifiedFile("README.md", `@@ -1,2 +1,2 @@
 # Example
-Run  make. 
+Run make.`)}, want: []string{DeltaWhitespace}},
		{name: "text read by a tool is not docs", files: []github.FileDelta{modifiedFile("requirements.txt", `@@ -1,2 +1,2 @@
 example-lib==1.0
-example-cli==2.0
+example-cli==2.1`)}},

		{name: "trailing spaces and a re-indented ruby line", files: []github.FileDelta{rubyWhitespace}, want: []string{DeltaWhitespace}},
		{name: "re-indented yaml line", files: []github.FileDelta{modifiedFile("config/example.yml", `@@ -1,2 +1,2 @@
 server:
-  port: 1
+    port: 1`)}},
		{name: "yaml trailing spaces", files: []github.FileDelta{modifiedFile("config/example.yml", `@@ -1,2 +1,2 @@
 server:
-  port:   1  
+  port: 1`)}, want: []string{DeltaWhitespace}},
		{name: "blank line added", files: []github.FileDelta{modifiedFile("app/models/example.rb", `@@ -1,3 +1,4 @@
 def call
+
   run
 end`)}, want: []string{DeltaWhitespace}},
		{name: "line moved past context", files: []github.FileDelta{modifiedFile("app/models/example.rb", `@@ -1,4 +1,4 @@
 def call
-  charge
   notify
+  charge
 end`)}},
		{name: "lines swapped", files: []github.FileDelta{modifiedFile("app/models/example.rb", `@@ -1,4 +1,4 @@
 def call
-  charge
-  notify
+  notify
+  charge
 end`)}},
		{name: "whitespace in a file without comment syntax", files: []github.FileDelta{modifiedFile("config/example.json", `@@ -1,3 +1,3 @@
 {
-  "port":   1 
+  "port": 1
 }`)}, want: []string{DeltaWhitespace}},
		{name: "code in a file without comment syntax", files: []github.FileDelta{modifiedFile("config/example.json", `@@ -1,3 +1,3 @@
 {
-  "port": 1
+  "port": 2
 }`)}},

		{name: "truncated", files: []github.FileDelta{func() github.FileDelta { f := goComment; f.Truncated = true; return f }()}},
		{name: "binary", files: []github.FileDelta{modifiedFile("app/assets/logo.png", "")}},
		{name: "renamed", files: []github.FileDelta{func() github.FileDelta {
			f := withStatus(goComment, "renamed")
			f.PreviousPath = "internal/example/old.go"
			return f
		}()}},
		{name: "added", files: []github.FileDelta{withStatus(goComment, "added")}},
		{name: "removed", files: []github.FileDelta{withStatus(goComment, "removed")}},
		{name: "mode changed", files: []github.FileDelta{withStatus(goComment, "changed")}},

		{name: "go line comment", files: []github.FileDelta{goComment}, want: []string{DeltaComments}},
		{name: "go block comment over added lines", files: []github.FileDelta{modifiedFile("internal/example/limit.go", `@@ -1,2 +1,6 @@
 package example
+/*
+Limit caps the batch.
+It is tuned for staging.
+*/
 const Limit = 10`)}, want: []string{DeltaComments}},
		{name: "js doc comment", files: []github.FileDelta{modifiedFile("app/javascript/example.js", `@@ -1,2 +1,5 @@
 import example from "./example";
+/**
+ * Shows the banner.
+ */
 export function show() {}`)}, want: []string{DeltaComments}},
		{name: "hunk starting inside a block", files: []github.FileDelta{modifiedFile("internal/example/limit.go", `@@ -10,4 +10,4 @@
  * Limit caps the batch.
- * Tuned for March.
+ * Tuned for September.
  */`)}, want: []string{DeltaComments}},
		{name: "pointer store", files: []github.FileDelta{modifiedFile("internal/example/set.go", `@@ -1,3 +1,3 @@
 func set(p *int, v int) {
-*p = 0
+*p = v
 }`)}},
		{name: "comment before code on one line", files: []github.FileDelta{modifiedFile("internal/example/run.go", `@@ -1,3 +1,3 @@
 func run() {
-x := 1
+/* c */ x := 1
 }`)}},
		{name: "code commented out", files: []github.FileDelta{modifiedFile("internal/example/run.go", `@@ -1,4 +1,6 @@
 func run() {
+/*
 charge()
+*/
 notify()
 }`)}},
		{name: "code commented back in", files: []github.FileDelta{modifiedFile("internal/example/run.go", `@@ -1,6 +1,4 @@
 func run() {
-/*
 charge()
-*/
 notify()
 }`)}},
		{name: "go directive", files: []github.FileDelta{modifiedFile("internal/example/files.go", `@@ -1,2 +1,3 @@
 package example
+//go:embed testdata
 var files embed.FS`)}},
		{name: "product continued after a closed comment", files: []github.FileDelta{modifiedFile("app/javascript/total.js", `@@ -1,3 +1,3 @@
 /* totals */
 const total = price
-  * qty;
+  * qty * 2;`)}},

		{name: "sql comment", files: []github.FileDelta{modifiedFile("db/example.sql", `@@ -1,3 +1,3 @@
 SELECT id
-  -- old filter note
+  -- active rows only
 FROM accounts`)}, want: []string{DeltaComments}},
		{name: "sql double minus without a space", files: []github.FileDelta{modifiedFile("db/example.sql", `@@ -1,2 +1,3 @@
 SELECT 5
+--1
 FROM accounts`)}},
		{name: "html comment", files: []github.FileDelta{modifiedFile("public/example.html", `@@ -1,2 +1,3 @@
 <div>
+  <!-- note -->
   <p>Hi</p>`)}, want: []string{DeltaComments}},
		{name: "vue comment block", files: []github.FileDelta{modifiedFile("app/javascript/Banner.vue", `@@ -1,2 +1,5 @@
 <template>
+  <!--
+    The banner shows only on staging.
+  -->
   <div class="banner"></div>`)}, want: []string{DeltaComments}},
		{name: "erb comment", files: []github.FileDelta{modifiedFile("app/views/example/show.html.erb", `@@ -1,2 +1,3 @@
 <div>
+  <%# note %>
   <%= render "banner" %>`)}, want: []string{DeltaComments}},
		{name: "erb tag inside an html comment", files: []github.FileDelta{modifiedFile("app/views/example/show.html.erb", `@@ -1,2 +1,3 @@
 <div>
+  <!-- <%= debug_info %> -->
   <%= render "banner" %>`)}},

		{name: "shebang", files: []github.FileDelta{modifiedFile("bin/example.sh", `@@ -1,2 +1,2 @@
-#!/bin/sh
+#!/bin/bash
 echo ok`)}},
		{name: "ruby magic comment", files: []github.FileDelta{modifiedFile("app/models/example.rb", `@@ -1,2 +1,3 @@
+# frozen_string_literal: true
 class Example
 end`)}},
		{name: "gemfile comment", files: []github.FileDelta{modifiedFile("Gemfile", `@@ -1,2 +1,3 @@
 source "https://rubygems.org"
+# Pinned until the next upgrade.
 gem "rails"`)}, want: []string{DeltaComments}},
		{name: "dockerfile variant comment", files: []github.FileDelta{modifiedFile("docker/Dockerfile.dev", `@@ -1,2 +1,3 @@
 FROM ruby:3.4
+# Development image.
 WORKDIR /app`)}, want: []string{DeltaComments}},
		{name: "makefile comment", files: []github.FileDelta{modifiedFile("Makefile", `@@ -1,2 +1,3 @@
 test:
+# runs everything
 	go test ./...`)}},

		{name: "comments and a blank line", files: []github.FileDelta{rubyCommentAndBlank}, want: []string{DeltaComments, DeltaWhitespace}},
		{name: "comments and a blank line, comments allowed", files: []github.FileDelta{rubyCommentAndBlank}, allowed: []string{DeltaComments}},
		{name: "every class over several files", files: []github.FileDelta{goComment, rubyWhitespace, readme},
			want: []string{DeltaComments, DeltaDocs, DeltaWhitespace}},
		{name: "one non-trivial file", files: []github.FileDelta{goComment, docsYAML, withStatus(readme, "added")}},
		{name: "nothing allowed", files: []github.FileDelta{goComment}, allowed: []string{}},
		{name: "no files", files: nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			allowed := tc.allowed
			if allowed == nil {
				allowed = DeltaClasses
			}
			got, ok := TrivialDelta(tc.files, allowed)
			if ok != (tc.want != nil) || !slices.Equal(got, tc.want) {
				t.Fatalf("TrivialDelta = %v, %v; want %v, %v", got, ok, tc.want, tc.want != nil)
			}
		})
	}
}

func TestDeltaLabel(t *testing.T) {
	cases := []struct {
		classes []string
		want    string
	}{
		{nil, ""},
		{[]string{"unknown"}, ""},
		{[]string{DeltaComments}, "comments only"},
		{[]string{DeltaWhitespace}, "whitespace only"},
		{[]string{DeltaDocs}, "docs only"},
		{[]string{DeltaComments, DeltaWhitespace}, "comments and whitespace only"},
		{[]string{DeltaWhitespace, DeltaComments}, "comments and whitespace only"},
		{[]string{DeltaComments, DeltaDocs}, "comments and docs only"},
		{[]string{DeltaDocs, DeltaComments}, "comments and docs only"},
		{[]string{DeltaWhitespace, DeltaDocs}, "whitespace and docs only"},
		{[]string{DeltaDocs, DeltaWhitespace}, "whitespace and docs only"},
		{[]string{DeltaComments, DeltaWhitespace, DeltaDocs}, "comments, whitespace and docs only"},
		{[]string{DeltaDocs, DeltaWhitespace, DeltaComments}, "comments, whitespace and docs only"},
		{[]string{DeltaWhitespace, DeltaDocs, DeltaComments, DeltaDocs}, "comments, whitespace and docs only"},
	}
	for _, tc := range cases {
		if got := DeltaLabel(tc.classes); got != tc.want {
			t.Errorf("DeltaLabel(%v) = %q, want %q", tc.classes, got, tc.want)
		}
	}
}
