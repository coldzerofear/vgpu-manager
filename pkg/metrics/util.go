package metrics

func GetValidValue(x uint32) uint32 {
	if x >= 0 && x <= 100 {
		return x
	}
	return 0
}

func CodecNormalize(x uint32) uint32 {
	return x * 85 / 100
}

func GetPercentageValue(x uint32) uint32 {
	switch {
	case x > 100:
		return 100
	case x < 0:
		return 0
	default:
		return x
	}
}
