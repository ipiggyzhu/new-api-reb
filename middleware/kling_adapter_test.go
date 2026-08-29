package middleware

import (
	"testing"

	"github.com/QuantumNous/new-api/common"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestKlingRequestConvertRewriteVisibleDownstream(t *testing.T) {
	c, _ := newJSONTestContext(t, "POST", "/kling/v1/videos/text2video",
		`{"model_name":"kling-v1","prompt":"a cat","aspect_ratio":"16:9","cfg_scale":0.9}`)

	KlingRequestConvert()(c)
	defer common.CleanupBodyStorage(c)

	assert.Equal(t, "/v1/video/generations", c.Request.URL.Path)

	// Distribute resolves the model from the cached body storage.
	storage, err := common.GetBodyStorage(c)
	require.NoError(t, err)
	raw, err := storage.Bytes()
	require.NoError(t, err)
	var envelope map[string]any
	require.NoError(t, common.Unmarshal(raw, &envelope))
	assert.Equal(t, "kling-v1", envelope["model"])
	assert.Equal(t, "a cat", envelope["prompt"])

	// The task handler re-parses the body via UnmarshalBodyReusable and needs
	// the original Kling fields preserved under metadata.
	var reparsed struct {
		Model    string         `json:"model"`
		Prompt   string         `json:"prompt"`
		Metadata map[string]any `json:"metadata"`
	}
	require.NoError(t, common.UnmarshalBodyReusable(c, &reparsed))
	assert.Equal(t, "kling-v1", reparsed.Model)
	assert.Equal(t, "a cat", reparsed.Prompt)
	require.NotNil(t, reparsed.Metadata)
	assert.Equal(t, "16:9", reparsed.Metadata["aspect_ratio"])
	assert.Equal(t, 0.9, reparsed.Metadata["cfg_scale"])
}

func TestKlingRequestConvertAcceptsModelField(t *testing.T) {
	c, _ := newJSONTestContext(t, "POST", "/kling/v1/videos/text2video",
		`{"model":"kling-v1-6","prompt":"a dog"}`)

	KlingRequestConvert()(c)
	defer common.CleanupBodyStorage(c)

	storage, err := common.GetBodyStorage(c)
	require.NoError(t, err)
	raw, err := storage.Bytes()
	require.NoError(t, err)
	var envelope map[string]any
	require.NoError(t, common.Unmarshal(raw, &envelope))
	assert.Equal(t, "kling-v1-6", envelope["model"])
}
