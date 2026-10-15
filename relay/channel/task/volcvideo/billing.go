package volcvideo

// estimateTokens implements Volcengine Ark's billing-token formula for video
// generation: tokens = seconds × W × H × 24 / 1024, where W×H is the maximum
// pixel count of the requested resolution tier (16:9 reference frame).
func estimateTokens(seconds int, resolution string) float64 {
	w, h := resolutionMaxPixels(resolution)
	return float64(seconds) * float64(w) * float64(h) * 24 / 1024
}

// resolutionMaxPixels returns the maximum pixel dimensions Ark bills for a
// resolution tier, using the 16:9 reference frame for each tier. An absent or
// unrecognized resolution bills as 1080p so a missing/unknown value never
// underbills relative to the documented tiers.
func resolutionMaxPixels(resolution string) (width, height int) {
	switch resolution {
	case "480p":
		return 854, 480
	case "720p":
		return 1280, 720
	case "1080p":
		return 1920, 1080
	case "4k":
		return 3840, 2160
	default:
		return 1920, 1080
	}
}

// requestSeconds derives the billable clip duration from the submit body.
// An absent duration defaults to 5s (the shortest Ark clip). duration=-1
// means "let the model choose the length"; Ark's longest clip is 15s, so that
// is used as the worst-case pre-charge estimate. Any other value is the
// requested integer number of seconds (already bounds-checked by
// ValidateRequestAndSetAction).
func requestSeconds(body map[string]any) int {
	raw, ok := body["duration"]
	if !ok {
		return 5
	}
	v, ok := raw.(float64)
	if !ok {
		return 5
	}
	if int(v) == -1 {
		return 15
	}
	return int(v)
}
