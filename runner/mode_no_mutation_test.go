// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package runner

import (
	"testing"

	"google.golang.org/genai"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/agent/llmagent"
	"google.golang.org/adk/v2/internal/llminternal"
	"google.golang.org/adk/v2/session"
)

// The last of the five writes this change removes, and the only one nothing
// else can see gone. Run used to stamp ModeChat onto an undeclared root's
// shared State. Every reader treats chat and unset alike, so no value assertion
// elsewhere can tell the write from its absence, and no concurrent test drives
// Run, so the race detector cannot either.
//
// It lives in package runner on purpose. The property is about this package's
// behaviour, and a version of it in another package leaves `go test ./runner/`
// green while the write is back — which is the loop someone editing this file
// actually runs.
func TestRun_DoesNotMutateTheRootAgentsMode(t *testing.T) {
	t.Parallel()

	root, err := llmagent.New(llmagent.Config{
		Name:  "root",
		Model: &scriptedModel{replyFmt: "reply %d"},
	})
	if err != nil {
		t.Fatalf("llmagent.New: %v", err)
	}
	internalRoot, ok := root.(llminternal.Agent)
	if !ok {
		t.Fatal("root is not an llminternal.Agent")
	}
	if got := llminternal.Reveal(internalRoot).Mode; got != llminternal.ModeUnset {
		t.Fatalf("precondition: declared mode = %q, want unset", got)
	}

	r, err := New(Config{
		AppName:           "app",
		Agent:             root,
		SessionService:    session.InMemoryService(),
		AutoCreateSession: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	var events int
	for _, err := range r.Run(t.Context(), "u", "s1", genai.NewContentFromText("hi", genai.RoleUser), agent.RunConfig{}) {
		if err != nil {
			t.Fatalf("Run: %v", err)
		}
		events++
	}
	if events == 0 {
		t.Fatal("the run produced no events, so it may not have reached the root bind")
	}

	if got := llminternal.Reveal(internalRoot).Mode; got != llminternal.ModeUnset {
		t.Errorf("declared mode after Run = %q, want unset (a run must not mutate the agent)", got)
	}
}
