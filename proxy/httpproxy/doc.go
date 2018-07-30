// Copyright 2015 The etcd Authors
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

// Package httpproxy implements etcd httpproxy. The etcd proxy acts as a reverse
// http proxy forwarding client requests to active etcd cluster members, and does
// not participate in consensus.
// etcd proxy作为一个反向http代理，将用户请求转发到活跃的etcd cluster members，并且不参与共识
package httpproxy
