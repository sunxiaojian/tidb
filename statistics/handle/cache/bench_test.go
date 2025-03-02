// Copyright 2023 PingCAP, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a Copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package cache

import (
	"math/rand"
	"sync"
	"testing"

	"github.com/pingcap/tidb/config"
	"github.com/pingcap/tidb/statistics"
	"github.com/pingcap/tidb/statistics/handle/cache/internal/testutil"
	"github.com/pingcap/tidb/util/benchdaily"
)

func benchCopyAndUpdate(b *testing.B, c *StatsCachePointer) {
	var wg sync.WaitGroup
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			t1 := testutil.NewMockStatisticsTable(1, 1, true, false, false)
			t1.PhysicalID = rand.Int63()
			cache := c.Load()
			c.Replace(cache.CopyAndUpdate([]*statistics.Table{t1}, nil))
		}()
	}
	wg.Wait()
	b.StopTimer()
}

func BenchmarkStatsCacheLRUCopyAndUpdate(b *testing.B) {
	restore := config.RestoreFunc()
	defer restore()
	config.UpdateGlobal(func(conf *config.Config) {
		conf.Performance.EnableStatsCacheMemQuota = true
	})
	benchCopyAndUpdate(b, NewStatsCachePointer())
}

func BenchmarkStatsCacheMapCacheCopyAndUpdate(b *testing.B) {
	restore := config.RestoreFunc()
	defer restore()
	config.UpdateGlobal(func(conf *config.Config) {
		conf.Performance.EnableStatsCacheMemQuota = false
	})
	benchCopyAndUpdate(b, NewStatsCachePointer())
}

func TestBenchDaily(t *testing.T) {
	benchdaily.Run(
		BenchmarkStatsCacheLRUCopyAndUpdate,
		BenchmarkStatsCacheMapCacheCopyAndUpdate,
	)
}
