package router

// ZCode 设备身份（deviceMid）的读取与注入。
//
// 官方 CLI 对每个 anthropic 供应商都会把设备身份写进请求体的
// metadata.user_id（ZCode 的 anthropic-request-metadata.ts），而
// X-Device-Mid 头只用于 billing/balance 与 feedback 端点——服务端不靠
// 它给模型调用鉴权。本文件只复刻前者的请求体形状。
//
// deviceMid 的生命周期归 ZCode 客户端：官方 CLI 与桌面端在
// ~/.zcode/v2/telemetry-state.json 里共用同一个字段。这里只读不生成，
// 与官方 sourceHeaders.ts 的 readExistingDeviceMid 同语义——机器上没跑过
// ZCode 时就没有设备身份，此时不注入该字段，而不是伪造一个 UUID。

import (
	"os"
	"path/filepath"
	"strings"

	json "github.com/goccy/go-json"
)

// zcodeDeviceStateName 是 ZCode 客户端存放设备身份的状态文件。
const zcodeDeviceStateName = "telemetry-state.json"

// zcodeDeviceStatePath 返回 ZCode 设备状态文件路径。ZCODE_DATA_BASE_DIR
// 重定向数据目录时跟随，与官方 cli-device-mid.ts 的解析顺序一致。
func zcodeDeviceStatePath() string {
	if base := strings.TrimSpace(os.Getenv(zcodeDataBaseDirEnv)); base != "" {
		return filepath.Join(base, ".zcode", "v2", zcodeDeviceStateName)
	}
	home := zcodeHomeDir()
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".zcode", "v2", zcodeDeviceStateName)
}

// zcodeDeviceMid 读取 ZCode 已写入的 deviceMid。文件缺失、损坏或字段为空
// 时返回空串——官方把设备标识的生成交给客户端，网关不代劳。
func zcodeDeviceMid() string {
	path := zcodeDeviceStatePath()
	if path == "" {
		return ""
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var state struct {
		DeviceMid string `json:"deviceMid"`
	}
	if json.Unmarshal(raw, &state) != nil {
		return ""
	}
	return strings.TrimSpace(state.DeviceMid)
}

// zcodeSessionIDFor 取本次请求的会话 id，供 metadata.user_id 与
// x-session-id / x-session-affinity 共用同一来源：两者必须同值，否则上游
// 看到的是同一会话的两个标识。翻译后的 payload 已把 Chat 的 messages 换成
// Messages 形状，而首个 user turn 在两条链路上都存在，故取 messages 即可。
func zcodeSessionIDFor(payload map[string]any) string {
	messages, _ := payload["messages"].([]any)
	return deriveZcodeRequestIDs(messages).session
}

// zcodeMetadataUserID 构造 Anthropic metadata.user_id 的值，形状取自官方
// anthropic-request-metadata.ts：device_id 取设备身份，account_uuid 官方
// 恒发空串，session_id 与已派生的会话 id 同源（同会话稳定、跨会话互异）。
//
// 返回空串表示本次不注入——调用方据此跳过 metadata，而不是发一个空对象。
func zcodeMetadataUserID(sessionID string) string {
	deviceMid := zcodeDeviceMid()
	if deviceMid == "" {
		return ""
	}
	encoded, err := json.Marshal(map[string]string{
		"device_id":    deviceMid,
		"account_uuid": "",
		"session_id":   sessionID,
	})
	if err != nil {
		return ""
	}
	return string(encoded)
}
