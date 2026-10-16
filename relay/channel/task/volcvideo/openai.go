package volcvideo

import (
	"errors"
	"fmt"
	"maps"
	"math"
	"slices"
	"strconv"
	"strings"

	"github.com/QuantumNous/new-api/common"

	"github.com/gin-gonic/gin"
)

// openAIVideoRatios are the Seedance aspect ratios an OpenAI "size" may map to.
var openAIVideoRatios = []struct {
	name  string
	value float64
}{
	{"16:9", 16.0 / 9}, {"4:3", 4.0 / 3}, {"1:1", 1}, {"3:4", 3.0 / 4}, {"9:16", 9.0 / 16}, {"21:9", 21.0 / 9},
}

// openAIOnlyKeys are OpenAI Videos fields that have no Volcengine meaning and
// are consumed by the conversion instead of being forwarded upstream.
var openAIOnlyKeys = []string{"prompt", "seconds", "size", "input_reference", "image", "images", "metadata"}

// normalizeOpenAIVideoRequest converts an OpenAI Videos style body
// ({model, prompt, seconds?, size?, input_reference?, metadata?}) into the
// Volcengine-native body the upstream accepts. A body that already carries
// "content" is native and returned unchanged.
//
// metadata holds Volcengine passthrough fields (resolution, ratio, duration,
// generate_audio, watermark, content, ...). Explicit top-level native fields
// win over metadata; seconds and size only fill duration, resolution and
// ratio when those are still unset.
func normalizeOpenAIVideoRequest(body map[string]any) (map[string]any, error) {
	if _, native := body["content"]; native {
		return body, nil
	}
	if _, openAI := body["prompt"]; !openAI {
		return body, nil
	}

	out := make(map[string]any, len(body))
	metadata, err := openAIVideoMetadata(body["metadata"])
	if err != nil {
		return nil, err
	}
	maps.Copy(out, metadata)
	for key, value := range body {
		if !slices.Contains(openAIOnlyKeys, key) {
			out[key] = value
		}
	}

	content := make([]any, 0, 4)
	prompt, ok := body["prompt"].(string)
	if !ok {
		return nil, errors.New("prompt must be a string")
	}
	if strings.TrimSpace(prompt) != "" {
		content = append(content, map[string]any{"type": "text", "text": prompt})
	}
	for _, key := range []string{"input_reference", "image"} {
		if url, isString := body[key].(string); isString && strings.TrimSpace(url) != "" {
			content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{"url": strings.TrimSpace(url)}})
		}
	}
	if images, isList := body["images"].([]any); isList {
		for _, image := range images {
			if url, isString := image.(string); isString && strings.TrimSpace(url) != "" {
				content = append(content, map[string]any{"type": "image_url", "image_url": map[string]any{"url": strings.TrimSpace(url)}})
			}
		}
	}
	if extra, isList := out["content"].([]any); isList {
		content = append(content, extra...)
	}
	out["content"] = content

	if _, set := out["duration"]; !set {
		if raw, present := body["seconds"]; present {
			seconds, err := openAIVideoSeconds(raw)
			if err != nil {
				return nil, err
			}
			out["duration"] = float64(seconds)
		}
	}
	if size, isString := body["size"].(string); isString && strings.TrimSpace(size) != "" {
		resolution, ratio, ok := openAIVideoSize(size)
		if !ok {
			return nil, fmt.Errorf("size must look like 1280x720, got %q", size)
		}
		if _, set := out["resolution"]; !set {
			out["resolution"] = resolution
		}
		if _, set := out["ratio"]; !set && ratio != "" {
			out["ratio"] = ratio
		}
	}
	return out, nil
}

// openAIVideoMetadata accepts metadata as an object or, as multipart clients
// send it, a JSON object string.
func openAIVideoMetadata(raw any) (map[string]any, error) {
	switch value := raw.(type) {
	case nil:
		return nil, nil
	case map[string]any:
		return value, nil
	case string:
		if strings.TrimSpace(value) == "" {
			return nil, nil
		}
		var parsed map[string]any
		if err := common.UnmarshalJsonStr(value, &parsed); err != nil || parsed == nil {
			return nil, errors.New("metadata must be a JSON object")
		}
		return parsed, nil
	default:
		return nil, errors.New("metadata must be a JSON object")
	}
}

// openAIVideoSeconds parses OpenAI's seconds, sent as a string ("8") or a number.
func openAIVideoSeconds(raw any) (int, error) {
	var seconds float64
	switch value := raw.(type) {
	case float64:
		seconds = value
	case string:
		parsed, err := strconv.ParseFloat(strings.TrimSpace(value), 64)
		if err != nil {
			return 0, errors.New("seconds must be an integer")
		}
		seconds = parsed
	default:
		return 0, errors.New("seconds must be an integer")
	}
	if seconds != math.Trunc(seconds) || math.IsInf(seconds, 0) {
		return 0, errors.New("seconds must be an integer")
	}
	return int(seconds), nil
}

// openAIVideoSize maps an OpenAI "WxH" size to a Seedance resolution tier and,
// when the aspect matches one Seedance offers, its ratio name.
func openAIVideoSize(size string) (resolution, ratio string, ok bool) {
	normalized := strings.ToLower(strings.ReplaceAll(strings.TrimSpace(size), "*", "x"))
	widthText, heightText, found := strings.Cut(normalized, "x")
	if !found {
		return "", "", false
	}
	width, errW := strconv.Atoi(strings.TrimSpace(widthText))
	height, errH := strconv.Atoi(strings.TrimSpace(heightText))
	if errW != nil || errH != nil || width <= 0 || height <= 0 {
		return "", "", false
	}
	switch longest := max(width, height); {
	case longest >= 3840:
		resolution = "4k"
	case longest >= 1920:
		resolution = "1080p"
	case longest >= 1280:
		resolution = "720p"
	default:
		resolution = "480p"
	}
	aspect := float64(width) / float64(height)
	for _, candidate := range openAIVideoRatios {
		if math.Abs(aspect-candidate.value)/candidate.value < 0.02 {
			ratio = candidate.name
			break
		}
	}
	return resolution, ratio, true
}

// multipartRequestBody reads an OpenAI Videos multipart submission. The
// upstream only accepts media by URL, so uploaded files are rejected; text
// fields become a JSON-like body for normalizeOpenAIVideoRequest.
func multipartRequestBody(c *gin.Context) (map[string]any, error) {
	form, err := common.ParseMultipartFormReusable(c)
	if err != nil {
		return nil, fmt.Errorf("invalid multipart form: %w", err)
	}
	defer form.RemoveAll()
	if len(form.File) > 0 {
		return nil, errors.New("file uploads are not supported; pass image URLs in input_reference or metadata.content")
	}
	body := make(map[string]any, len(form.Value))
	for key, values := range form.Value {
		if len(values) != 1 {
			return nil, fmt.Errorf("%s must be provided once", key)
		}
		body[key] = values[0]
	}
	return body, nil
}
