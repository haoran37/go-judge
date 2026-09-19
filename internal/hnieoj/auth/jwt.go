package auth

import (
	"encoding/base64"
	"encoding/json"
	"strconv"
)

func base64URLDecode(value string) ([]byte, error) {
	raw, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil {
		return base64.StdEncoding.DecodeString(value)
	}
	return raw, nil
}

func jsonUnmarshal(data []byte, dst any) error {
	return json.Unmarshal(data, dst)
}

func normalizeEpoch(value any) int64 {
	var epoch int64
	switch v := value.(type) {
	case float64:
		epoch = int64(v)
	case json.Number:
		parsed, err := v.Int64()
		if err != nil {
			return 0
		}
		epoch = parsed
	case string:
		parsed, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return 0
		}
		epoch = parsed
	default:
		return 0
	}
	if epoch <= 0 {
		return 0
	}
	// 后端 JWT 使用 Unix 秒；兼容毫秒签发实现的本地调度。
	if epoch > 1_000_000_000_000 {
		return epoch
	}
	return epoch * 1000
}
