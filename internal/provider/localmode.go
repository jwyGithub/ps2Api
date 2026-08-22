package provider

import (
	"encoding/json"
	"fmt"
	"strings"
)

// desktopLocalModeExcludedTools 取自真实 localmode 桌面会话抓包里 clientTools.excludedTools。
// 它只是「隐藏这些工具不给模型」的客户端清单,不影响 executeShellCommand 等本地工具的可用性;
// 原样对齐是为了让网关 desktop 请求与已验证能跑 shell 的抓包一致,减少实测变量。
var desktopLocalModeExcludedTools = []string{
	"listDatasets", "createDataset", "previewDataset", "queryDatasetView", "deleteDataset",
	"getDatasetSchema", "createDatasetView", "deleteDatasetView", "runQuery", "insertDatasetRows",
	"modifyDatasetView", "refreshDatasource", "addDatasetSource", "editDatasetSource",
	"removeDatasetSource", "testDatasourceConnection", "readDatasetRunForScenario",
	"attachDatasetToScenario", "setDatasetInputMapping", "setDatasetInputLiteral",
	"clearDatasetInputMapping", "runFlowWithDataset", "listCloudCodeMocks", "getCloudCodeMock",
	"createCloudCodeMock", "updateCloudCodeMock", "deleteCloudCodeMock", "deployMockServer",
	"listCloudMockServers", "getCloudMockServer", "getMockServerLogs", "getMockServerState",
	"setMockServerEnableSession", "clearMockServerSession", "deleteMockServerStateKey",
	"updateMockServer", "unpublishMockServer", "deleteMockServer", "checkMockServerSlugAvailability",
	"createCloudSimulation", "listCloudSimulations", "getCloudSimulation", "updateCloudSimulation",
	"deleteCloudSimulation", "startCloudSimulation", "stopCloudSimulation", "getCloudSimulationLogs",
	"checkSimulationSlugAvailability", "validateMockForCloud", "publishMockToCloud",
	"runCollectionWithSimulation", "getCollectionRunWithSimulationResults", "configureScenarios",
	"startSimulation", "stopSimulation", "stopSimulationCollectionRun",
	"analyzeCollectionRunWithSimulationResults", "listSimulations", "getSimulationConfig",
	"createSimulationConfig", "updateSimulationConfig", "deleteSimulation", "askUser",
}

// desktopExcludedTools 是旧 api_catalog Web 分支曾用的 excludedTools 清单。Web 分支已切回
// workspace_v12 三元组(excludedTools 对齐真实浏览器的 ["askUser"]),此清单目前未被引用,
// 仅保留作参考/回滚依据。
var desktopExcludedTools = []string{
	"listDatasets", "createDataset", "previewDataset", "queryDatasetView", "deleteDataset",
	"getDatasetSchema", "createDatasetView", "deleteDatasetView", "runQuery", "insertDatasetRows",
	"modifyDatasetView", "refreshDatasource", "addDatasetSource", "editDatasetSource",
	"removeDatasetSource", "testDatasourceConnection", "listCloudMocks", "getCloudMock",
	"getCloudMockLogs", "renameCloudMock", "deleteCloudMock", "checkMockSlugAvailability",
	"createCloudMock", "listWorkspaceDocs", "getWorkspaceDoc", "createWorkspaceDoc",
	"updateWorkspaceDoc", "deleteWorkspaceDoc", "askUser",
}

type nativeToolResponse struct {
	conversationID string
	groupID        string
	responses      []map[string]interface{}
}

func (p *Provider) nativeToolResponse(accountID int64, messages []ChatMessage) (nativeToolResponse, bool) {
	toolIdx := toolTailIndex(messages)
	if toolIdx < 0 {
		return nativeToolResponse{}, false
	}
	response := nativeToolResponse{conversationID: p.LookupConversation(accountID, messages)}
	if response.conversationID == "" {
		return nativeToolResponse{}, false
	}
	add := func(toolCallID, content string, failed bool) bool {
		groupID := p.lookupToolGroup(accountID, toolCallID)
		if groupID == "" {
			return false
		}
		if response.groupID == "" {
			response.groupID = groupID
		} else if response.groupID != groupID {
			return false
		}
		status := "SUCCESS"
		if failed {
			status = "FAILED"
		}
		payload := strings.TrimSpace(content)
		if !json.Valid([]byte(payload)) {
			encoded, _ := json.Marshal(map[string]string{"status": status, "message": content})
			payload = string(encoded)
		}
		// 给 content 上限，避免续期时把出站 body 顶过 Cloudflare WAF 信封（触发 403）。
		// 保留头尾、中段截断,并修正可能被切断的 UTF-8。
		if len(payload) > MaxToolResponseContentLen {
			head := 512
			tail := MaxToolResponseContentLen - head - 32
			payload = strings.ToValidUTF8(payload[:head], "") +
				"\n...[tool result truncated]...\n" +
				strings.ToValidUTF8(payload[len(payload)-tail:], "")
		}
		entry := map[string]interface{}{
			"toolCallId":          toolCallID,
			"content":             payload,
			"toolResponseSummary": safeToolResponseSummary(status, content),
			"toolResponseStatus":  status,
		}
		if failed {
			entry["toolResponseFailureType"] = "UNHANDLED_ERROR"
		}
		response.responses = append([]map[string]interface{}{entry}, response.responses...)
		return true
	}
	for i := toolIdx; i >= 0; i-- {
		msg := messages[i]
		if msg.Role == "tool" {
			content := ExtractText(msg.Content)
			failed := strings.Contains(content, "<tool_use_error>")
			if !failed {
				var result struct {
					Status  string `json:"status"`
					IsError bool   `json:"is_error"`
				}
				if json.Unmarshal([]byte(content), &result) == nil {
					failed = result.IsError || strings.EqualFold(result.Status, "FAILED") || strings.EqualFold(result.Status, "ERROR")
				}
			}
			if !add(msg.ToolCallID, content, failed) {
				return nativeToolResponse{}, false
			}
			continue
		}
		if !isAnthropicToolResult(msg) {
			break
		}
		var blocks []map[string]interface{}
		if json.Unmarshal(msg.Content, &blocks) != nil {
			return nativeToolResponse{}, false
		}
		for i := len(blocks) - 1; i >= 0; i-- {
			block := blocks[i]
			if block["type"] != "tool_result" {
				continue
			}
			toolCallID, _ := block["tool_use_id"].(string)
			failed, _ := block["is_error"].(bool)
			if toolCallID == "" || !add(toolCallID, toolResultText(block["content"]), failed) {
				return nativeToolResponse{}, false
			}
		}
	}
	return response, len(response.responses) > 0 && response.groupID != ""
}

// safeToolResponseSummary mirrors Postman's short native summaries without copying
// source code, HTML, commands, or other tool output into a second request field.
func safeToolResponseSummary(status, content string) string {
	return fmt.Sprintf("Tool result: %s, %d bytes", status, len(content))
}
