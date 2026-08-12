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

// Package workflowstate hands a few facts about a rehydrated workflow run to
// the packages that drive it, without widening the workflow package's public
// API. It deliberately imports nothing: the workflow package installs the hooks
// below from its own init, and any package that reads them already imports
// workflow, so they are non-nil by the time they are called.
//
// The arguments are typed any because naming *workflow.RunState here would be
// an import cycle — workflow imports this package.
package workflowstate

// ActionableInterruptIDs reports the interrupt IDs a rehydrated run can still
// do something about: those a node is waiting for, those a re-entry node is
// about to be re-run with, and those a node settled on this very turn. A
// FunctionResponse matching none of them answers nothing in that run.
//
// Installed by workflow.init; the argument is a *workflow.RunState. Returns nil
// for anything else, which callers should read as "cannot tell" rather than
// "nothing is actionable".
var ActionableInterruptIDs func(runState any) map[string]struct{}
