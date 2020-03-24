// Copyright 2016 The etcd Authors
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

package embed

import (
	"path/filepath"

	"go.etcd.io/etcd/wal"
)

func isMemberInitialized(cfg *Config) bool {
	waldir := cfg.WalDir
	if waldir == "" {
		waldir = filepath.Join(cfg.Dir, "member", "wal")
	}
	// 根据wal目录是否存在来判断member是否已经被初始化了
	return wal.Exist(waldir)
}
