package watcher

type BatchConfig struct {
	StartIndex int
	EndIndex   int
	Count      int
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
