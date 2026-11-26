package privacy

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// OutboundLimit bounds the converted request this layer inspects.
const OutboundLimit = 8 << 20

// ProtectOutboundJSON runs before request shaping/signing and debug logs, so
// provider conversions and channel overrides cannot reintroduce credentials.
func ProtectOutboundJSON(c *gin.Context, data []byte) ([]byte, error) {
	session := FromContext(c)
	if session == nil {
		return data, nil
	}
	if !session.BestEffort {
		if err := ValidateTextRequest(data); err != nil {
			return nil, err
		}
	} else if !gjson.ValidBytes(data) {
		// A body converted to a form or to binary holds nothing readable here.
		return data, nil
	}
	return session.ProtectJSON(data, Options{ProtectIPs: session.ProtectIPs})
}

func ProtectReader(c *gin.Context, reader io.Reader) (io.Reader, error) {
	session := FromContext(c)
	if session == nil {
		return reader, nil
	}
	if reader == nil {
		return nil, errors.New("privacy requires a JSON request body")
	}
	data, err := io.ReadAll(io.LimitReader(reader, OutboundLimit+1))
	if err != nil {
		return nil, ErrLimit
	}
	if len(data) > OutboundLimit {
		if !session.BestEffort {
			return nil, ErrLimit
		}
		// Conversion inlines fetched media and can outgrow what is inspected.
		// The text in it was already substituted when the request arrived.
		rest, err := io.ReadAll(reader)
		if err != nil {
			return nil, ErrLimit
		}
		return bytes.NewReader(append(data, rest...)), nil
	}
	protected, err := ProtectOutboundJSON(c, data)
	if err != nil {
		clear(data)
		return nil, err
	}
	// A body returned as it came is the same memory and must stay intact.
	if len(protected) == 0 || &protected[0] != &data[0] {
		clear(data)
	}
	return bytes.NewReader(protected), nil
}

// CheckOutboundJSON is read-only: the already-shaped/signed request must not be
// rewritten. Known input secrets may never reappear in its content fields.
func CheckOutboundJSON(c *gin.Context, data []byte) error {
	session := FromContext(c)
	if session == nil {
		return nil
	}
	if session.BestEffort && !gjson.ValidBytes(data) {
		return nil
	}
	_, err := walkJSON(data, jsonField{}, 0, func(field jsonField, value string) (string, error) {
		if session.maskField(field, value) != value {
			return "", errors.New("privacy final request check failed")
		}
		return value, nil
	})
	return err
}

func ProtectHeaders(c *gin.Context, headers http.Header) error {
	session := FromContext(c)
	if session == nil {
		return nil
	}
	for key, values := range headers {
		switch strings.ToLower(key) {
		case "x-new-api-privacy", "x-new-api-privacy-session":
			delete(headers, key)
		case "authorization", "x-api-key", "api-key", "x-goog-api-key":
			// The selected provider's authentication is not prompt content.
		case "x-codex-turn-metadata":
			for i, value := range values {
				encoded, err := common.Marshal(value)
				if err != nil {
					return err
				}
				protected, err := session.maskJSON(encoded, jsonField{scope: toolData})
				if err != nil {
					return err
				}
				if err := common.Unmarshal(protected, &values[i]); err != nil {
					return err
				}
			}
		case "content-type":
			for i, value := range values {
				if separator := strings.IndexByte(value, ';'); separator >= 0 {
					values[i] = value[:separator+1] + session.Mask(value[separator+1:])
				}
			}
		default:
			for i, value := range values {
				values[i] = session.Mask(value)
			}
		}
	}
	return nil
}
