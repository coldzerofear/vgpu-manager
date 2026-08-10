/*
Copyright 2025-2026 coldzerofear

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    https://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package watcher

import "sync"

type BatchConfig struct {
	StartIndex int
	EndIndex   int
	Count      int
}

type BatchParallel struct {
	wg      sync.WaitGroup
	configs []BatchConfig
}

func NewBatchParallel(totalCards, maxPerBatch int) BatchParallel {
	return BatchParallel{
		configs: BalanceBatches(totalCards, maxPerBatch),
	}
}

func (b *BatchParallel) Execute(fn func(int, BatchConfig)) {
	if len(b.configs) == 1 {
		fn(0, b.configs[0])
		return
	}
	for i, config := range b.configs {
		b.wg.Add(1)
		go func(i int, config BatchConfig) {
			defer b.wg.Done()
			fn(i, config)
		}(i, config)
	}
}

func (b *BatchParallel) WaitDone() {
	if len(b.configs) <= 1 {
		return
	}
	b.wg.Wait()
}

func BalanceBatches(totalCards, maxPerBatch int) []BatchConfig {
	if totalCards <= 0 || maxPerBatch <= 0 {
		return nil
	}

	numBatches := (totalCards + maxPerBatch - 1) / maxPerBatch

	baseSize := totalCards / numBatches
	remainder := totalCards % numBatches

	batches := make([]BatchConfig, numBatches)
	currentIndex := 0

	for i := 0; i < numBatches; i++ {
		batchSize := baseSize
		if i < remainder {
			batchSize++
		}

		batches[i] = BatchConfig{
			StartIndex: currentIndex,
			EndIndex:   currentIndex + batchSize - 1,
			Count:      batchSize,
		}

		currentIndex += batchSize
	}

	return batches
}
