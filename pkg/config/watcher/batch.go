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

func (b *BatchParallel) Execute(fn func(BatchConfig)) {
	if len(b.configs) == 1 {
		fn(b.configs[0])
		return
	}
	for _, config := range b.configs {
		b.wg.Add(1)
		go func(config BatchConfig) {
			defer b.wg.Done()
			fn(config)
		}(config)
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
