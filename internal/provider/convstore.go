package provider

import (
	"strconv"
	"strings"
	"sync"
)

// ConversationStore 保存 Provider 的会话态：会话 ID 映射、会话归属账号（粘性路由用）、
// 以及 Postman 工具调用组 ID。抽成接口后默认给进程内内存实现；将来可替换为
// Redis/SQLite 以支持多实例保活，而调用方（conversation.go）无需任何改动。
//
// 约定：fingerprint 由 conversationFingerprint 计算，account:xxx 复合键的拼装
// 是实现细节，完全封装在实现内部，接口只暴露领域参数（accountID / fingerprint /
// toolCallID）。
type ConversationStore interface {
	// GetConversation 按 (账号, 消息指纹) 取回 Postman 会话 ID。
	GetConversation(accountID int64, fingerprint string) (string, bool)
	// PutConversation 记录 (账号, 消息指纹) -> 会话 ID。
	PutConversation(accountID int64, fingerprint, conversationID string)

	// GetOwner 按消息指纹取回会话首次使用的账号（粘性路由用）。
	GetOwner(fingerprint string) (int64, bool)
	// PutOwner 记录消息指纹 -> 归属账号。
	PutOwner(fingerprint string, accountID int64)

	// GetToolGroup 按 (账号, toolCallID) 取回 Postman 工具调用组 ID。
	GetToolGroup(accountID int64, toolCallID string) (string, bool)
	// PutToolGroup 记录 (账号, toolCallID) -> 组 ID。
	PutToolGroup(accountID int64, toolCallID, groupID string)

	// Reset 清空某账号名下的全部会话映射与归属映射。
	Reset(accountID int64)

	// Mode 返回人类可读的存储模式描述，供启动日志体现当前会话存储去向。
	Mode() string
}

// memoryConversationStore 是 ConversationStore 的进程内默认实现，基于 sync.Map，
// 语义与重构前逐字节保持一致。
type memoryConversationStore struct {
	convMap    sync.Map // accountID:fingerprint -> conversationID
	convOwn    sync.Map // fingerprint -> accountID(int64) 会话归属账号（粘性路由用）
	toolGroups sync.Map // accountID:toolCallID -> Postman toolCallGroupId
}

func newMemoryConversationStore() *memoryConversationStore {
	return &memoryConversationStore{}
}

// Mode 返回存储模式描述。内存实现进程内、不跨实例、随进程退出丢失。
func (m *memoryConversationStore) Mode() string {
	return "内存 (进程内, 不跨实例)"
}

func convKey(accountID int64, fp string) string {
	return strconv.FormatInt(accountID, 10) + ":" + fp
}

func toolGroupKey(accountID int64, toolCallID string) string {
	return strconv.FormatInt(accountID, 10) + ":" + toolCallID
}

func (m *memoryConversationStore) GetConversation(accountID int64, fingerprint string) (string, bool) {
	if v, ok := m.convMap.Load(convKey(accountID, fingerprint)); ok {
		return v.(string), true
	}
	return "", false
}

func (m *memoryConversationStore) PutConversation(accountID int64, fingerprint, conversationID string) {
	m.convMap.Store(convKey(accountID, fingerprint), conversationID)
}

func (m *memoryConversationStore) GetOwner(fingerprint string) (int64, bool) {
	if v, ok := m.convOwn.Load(fingerprint); ok {
		return v.(int64), true
	}
	return 0, false
}

func (m *memoryConversationStore) PutOwner(fingerprint string, accountID int64) {
	m.convOwn.Store(fingerprint, accountID)
}

func (m *memoryConversationStore) GetToolGroup(accountID int64, toolCallID string) (string, bool) {
	if v, ok := m.toolGroups.Load(toolGroupKey(accountID, toolCallID)); ok {
		return v.(string), true
	}
	return "", false
}

func (m *memoryConversationStore) PutToolGroup(accountID int64, toolCallID, groupID string) {
	m.toolGroups.Store(toolGroupKey(accountID, toolCallID), groupID)
}

func (m *memoryConversationStore) Reset(accountID int64) {
	prefix := strconv.FormatInt(accountID, 10) + ":"
	m.convMap.Range(func(k, _ any) bool {
		if s, ok := k.(string); ok && strings.HasPrefix(s, prefix) {
			m.convMap.Delete(k)
		}
		return true
	})
	m.convOwn.Range(func(k, v any) bool {
		if id, ok := v.(int64); ok && id == accountID {
			m.convOwn.Delete(k)
		}
		return true
	})
}
