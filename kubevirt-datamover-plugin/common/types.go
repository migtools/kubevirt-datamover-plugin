// Copyright 2026 Red Hat Inc.
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

// Package common contains shared types and constants for the kubevirt datamover plugin.
// Note: Volume policy checking is now done via velero's volumehelper package which
// reads policies from the resource policy configmap. The constants previously defined
// here for volume policy actions have been removed as they are no longer needed.
package common
