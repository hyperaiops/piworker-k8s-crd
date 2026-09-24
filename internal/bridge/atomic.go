/*
Copyright 2026.

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

package bridge

import "sync/atomic"

// atomicReady is a goroutine-safe readiness flag for the /readyz probe.
type atomicReady struct{ v atomic.Bool }

func (a *atomicReady) set(v bool) { a.v.Store(v) }
func (a *atomicReady) get() bool  { return a.v.Load() }
