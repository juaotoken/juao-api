package volcvideo

import (
	"fmt"
	"math"
	"slices"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	"github.com/QuantumNous/new-api/pkg/taskusage"
	relaycommon "github.com/QuantumNous/new-api/relay/common"

	"github.com/gin-gonic/gin"
)

// seedanceResolutions lists the output resolution tiers each model offers,
// matching the doubao task plugin's Seedance capability profiles
// (doubao-seedance-2-0-260128 / -fast-260128 / -mini-260615 / -2-5-260628).
// Every model accepts reference video input. The upstream drops the Ark
// endpoint suffix from the plugin's names, so each entry also covers the
// suffix-free spelling the 500 channel exposes.
var seedanceResolutions = map[string][]string{
	"doubao-seedance-2-0":      {"480p", "720p", "1080p", "4k"},
	"doubao-seedance-2-0-fast": {"480p", "720p"},
	"doubao-seedance-2-0-mini": {"480p", "720p"},
	"doubao-seedance-2-5":      {"480p", "720p", "1080p"},
}

// allSeedanceResolutions is the profile of an upstream model name this
// adaptor does not declare (for example a channel mapping to a vendor
// endpoint ID): every Seedance tier is accepted.
var allSeedanceResolutions = []string{"480p", "720p", "1080p", "4k"}

// seedanceModelPrefixes are the model-name families whose members are not all
// known in advance. The upstream keeps adding Seedance releases (3.0, 3.5, ...)
// under the same wire protocol, and a model the registry does not cover gets no
// usage schema, which makes its tiered pricing expression unsavable. Every name
// in these families therefore falls back to the shared profile below; the three
// declared models keep their narrower resolution lists through their exact
// registrations, which win over the prefix. A member that prices audio
// separately (Seedance 1.5 pro) still needs its own registration to expose a
// `generate_audio` field.
var seedanceModelPrefixes = []string{"doubao-seedance-"}

func init() {
	for _, name := range ModelList {
		resolutions := seedanceResolutions[name]
		taskusage.Register([]string{name}, seedanceUsageSchema(resolutions), seedanceUsageExamples(resolutions))
	}
	taskusage.RegisterPrefix(seedanceModelPrefixes,
		seedanceUsageSchema(allSeedanceResolutions), seedanceUsageExamples(allSeedanceResolutions))
}

// seedanceUsageSchema mirrors the doubao plugin's seedanceUsageSchema for the
// Seedance 2.0 profiles (token-priced, reference video input, no audio switch)
// so task expressions are interchangeable between the plugin and this adaptor.
func seedanceUsageSchema(resolutions []string) map[string]jsplugin.UsageFieldSchema {
	resolutionLabels := make(map[string]jsplugin.LocalizedText, len(resolutions))
	for _, resolution := range resolutions {
		resolutionLabels[resolution] = jsplugin.LocalizedText{"en": resolution, "zh": resolution}
	}
	return map[string]jsplugin.UsageFieldSchema{
		// Upstream billing tokens: estimated at submit, the polled
		// total_tokens on completion.
		"tokens": {
			Type:        "number",
			Unit:        "token",
			Description: jsplugin.LocalizedText{"en": "Billing token unit price", "zh": "计费 Token 单价"},
		},
		// Output video resolution; the token unit price varies by tier.
		"resolution": {
			Enum:        append([]string(nil), resolutions...),
			EnumLabels:  resolutionLabels,
			Description: jsplugin.LocalizedText{"en": "Output video resolution", "zh": "输出视频分辨率"},
		},
		// Seedance 2.x prices video-to-video tokens at a lower unit rate.
		"video_input": {
			Enum: []string{"none", "video"},
			EnumLabels: map[string]jsplugin.LocalizedText{
				"none":  {"en": "No reference video", "zh": "无参考视频"},
				"video": {"en": "With reference video", "zh": "有参考视频"},
			},
			Description: jsplugin.LocalizedText{"en": "Reference video input", "zh": "参考视频输入"},
		},
	}
}

// seedanceUsageExamples mirrors the plugin's display examples: a 5-second
// video at each tier, plus a 720p video with 4 seconds of reference video
// input (Ark bills input and output seconds together).
func seedanceUsageExamples(resolutions []string) []jsplugin.UsageExample {
	examples := make([]jsplugin.UsageExample, 0, len(resolutions)+1)
	for _, resolution := range resolutions {
		examples = append(examples, jsplugin.UsageExample{
			Label: resolution + " · 5s",
			Facts: map[string]any{"tokens": math.Round(estimateTokens(5, resolution)), "resolution": resolution, "video_input": "none"},
		})
	}
	examples = append(examples, jsplugin.UsageExample{
		Label: "720p · 5s (+4s 输入视频)",
		Facts: map[string]any{"tokens": math.Round(estimateTokens(9, "720p")), "resolution": "720p", "video_input": "video"},
	})
	return examples
}

// modelResolutions returns the resolution profile of the first declared
// candidate model name: the upstream name, then the client-facing name.
func modelResolutions(models ...string) []string {
	for _, name := range models {
		if resolutions, ok := seedanceResolutions[strings.TrimSpace(name)]; ok {
			return resolutions
		}
	}
	return allSeedanceResolutions
}

// ExtractUsageFacts implements channel.TaskUsageFactsProvider; the host uses
// the validated variant below when it is available.
func (a *Adaptor) ExtractUsageFacts(c *gin.Context, info *relaycommon.RelayInfo) map[string]any {
	facts, _ := a.ExtractUsageFactsValidated(c, info)
	return facts
}

// ExtractUsageFactsValidated returns the submit-time usage facts for task
// usage expressions (tiered billing), following the doubao plugin's
// semantics:
//   - resolution: the requested tier, or the tier of a WxH size, lowercased.
//     A request that leaves it to the model reserves the model's highest tier
//     up to 1080p. A tier the model does not offer is rejected.
//   - video_input: "video" when any content item carries a video_url.
//   - tokens: Ark's estimate for the requested duration (requestSeconds) at
//     the tier's maximum pixel count, the same estimate ratio mode reserves.
//
// Completion overlays the measured tokens and resolution.
func (a *Adaptor) ExtractUsageFactsValidated(c *gin.Context, info *relaycommon.RelayInfo) (map[string]any, error) {
	upstreamModel := ""
	if info.ChannelMeta != nil {
		upstreamModel = info.UpstreamModelName
	}
	resolutions := modelResolutions(upstreamModel, info.OriginModelName)
	raw, _ := a.requestBody["resolution"].(string)
	resolution, recognized := normalizeResolution(raw)
	if !recognized {
		resolution = resolutions[len(resolutions)-1]
		if slices.Contains(resolutions, "1080p") {
			resolution = "1080p"
		}
	}
	if !slices.Contains(resolutions, resolution) {
		modelName := upstreamModel
		if modelName == "" {
			modelName = info.OriginModelName
		}
		return nil, fmt.Errorf("%s resolution must be one of %s", modelName, strings.Join(resolutions, ", "))
	}
	videoInput := "none"
	if hasVideoInput(a.requestBody) {
		videoInput = "video"
	}
	seconds := requestSeconds(a.requestBody)
	tokens := min(estimateTokens(seconds, resolution), float64(common.MaxQuota))
	a.recordSubmitLogUsage(info, tokens)
	return map[string]any{
		"tokens":      tokens,
		"resolution":  resolution,
		"video_input": videoInput,
	}, nil
}

// normalizeResolution maps a Seedance tier name or a WxH size to a lowercase
// tier. recognized is false for an absent or unparseable value.
func normalizeResolution(value string) (resolution string, recognized bool) {
	raw := strings.ToLower(strings.TrimSpace(value))
	if slices.Contains(allSeedanceResolutions, raw) {
		return raw, true
	}
	if tier, _, ok := openAIVideoSize(raw); ok {
		return tier, true
	}
	return "", false
}

func hasVideoInput(body map[string]any) bool {
	content, _ := body["content"].([]any)
	return slices.ContainsFunc(content, func(item any) bool {
		part, ok := item.(map[string]any)
		if !ok {
			return false
		}
		if kind, _ := part["type"].(string); kind == "video_url" {
			return true
		}
		_, present := part["video_url"]
		return present
	})
}

// completionUsageFacts returns the measured facts of a successful poll:
// upstream total_tokens and the delivered resolution. Values that are missing
// or not offered by the task's model are omitted so the submit-time estimate
// stays in place.
func completionUsageFacts(task *model.Task, data pollData) map[string]any {
	facts := make(map[string]any, 2)
	if tokens := data.TotalTokens.Int(); tokens > 0 {
		facts["tokens"] = float64(tokens)
	}
	var models []string
	if task != nil {
		models = []string{task.Properties.UpstreamModelName, task.Properties.OriginModelName}
	}
	if resolution := strings.ToLower(strings.TrimSpace(data.Resolution)); slices.Contains(modelResolutions(models...), resolution) {
		facts["resolution"] = resolution
	}
	if len(facts) == 0 {
		return nil
	}
	return facts
}

// recordSubmitLogUsage publishes the estimated billing tokens and the
// requested video parameters on the submit consume log.
func (a *Adaptor) recordSubmitLogUsage(info *relaycommon.RelayInfo, tokens float64) {
	if info == nil || info.TaskRelayInfo == nil {
		return
	}
	other := map[string]any{"video_requested_seconds": requestSeconds(a.requestBody)}
	if resolution, _ := a.requestBody["resolution"].(string); strings.TrimSpace(resolution) != "" {
		other["video_resolution"] = strings.ToLower(strings.TrimSpace(resolution))
	}
	if ratio, _ := a.requestBody["ratio"].(string); strings.TrimSpace(ratio) != "" {
		other["video_ratio"] = strings.TrimSpace(ratio)
	}
	info.TaskRelayInfo.LogUsage = &relaycommon.TaskLogUsage{
		CompletionTokens: common.QuotaRound(tokens),
		Other:            other,
	}
}

// TaskSettlementLog implements service.TaskSettlementLogProvider: the
// settlement log carries the upstream billing tokens, the generation time from
// submission to completion, and the delivered video parameters from the
// sanitized poll snapshot.
func (a *Adaptor) TaskSettlementLog(task *model.Task, taskResult *relaycommon.TaskInfo) relaycommon.TaskLogUsage {
	usage := relaycommon.TaskLogUsage{Other: map[string]any{"is_task": true}}
	if task == nil {
		return usage
	}
	if taskResult != nil && taskResult.TotalTokens > 0 {
		usage.CompletionTokens = taskResult.TotalTokens
	}
	if task.SubmitTime > 0 && task.FinishTime >= task.SubmitTime {
		usage.UseTimeSeconds = int(min(task.FinishTime-task.SubmitTime, int64(common.MaxQuota)))
		usage.Other["video_generation_seconds"] = usage.UseTimeSeconds
	}
	var snapshot struct {
		Duration   tokenCount `json:"duration"`
		Resolution string     `json:"resolution"`
		Ratio      string     `json:"ratio"`
	}
	if len(task.Data) > 0 {
		_ = common.Unmarshal(task.Data, &snapshot)
	}
	if seconds := snapshot.Duration.Int(); seconds > 0 && seconds <= relaycommon.MaxTaskDurationSeconds {
		usage.Other["video_seconds"] = seconds
	}
	if resolution := strings.TrimSpace(snapshot.Resolution); resolution != "" {
		usage.Other["video_resolution"] = strings.ToLower(resolution)
	}
	if ratio := strings.TrimSpace(snapshot.Ratio); ratio != "" {
		usage.Other["video_ratio"] = ratio
	}
	return usage
}
