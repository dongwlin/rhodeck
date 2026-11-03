package maa

import (
	"bytes"
	"encoding/json"
)

// Event 是一条公开事件:MessageID 是回调消息类型(AsstCallback 的
// message 字段,与 TaskID、AsyncID 互不混用),JSON 是回调消息的
// 原始 JSON 原文(独立字节副本,可安全长期持有、修改,不影响
// 内部缓存),Sequence 是本实例内的本地事件序号(单调递增;
// 序号分配与耗尽语义由分发器承接)。
type Event struct {
	MessageID MessageID
	JSON      json.RawMessage
	Sequence  uint64
}

// AsyncResult 是一次异步提交的最终结果:ID 是关联提交时的 AsyncID,
// What 是结果类型文本,OK 对应原生 C bool 返回(false 表示原生
// 操作失败,由 Wait 另行返回可辨识的错误,不在本字段编造结构化
// 原因),Details 是原始 details JSON 原文(独立字节副本,可安全
// 长期持有、修改)。
type AsyncResult struct {
	ID      AsyncID
	What    string
	OK      bool
	Details json.RawMessage
}

type eventRecord struct {
	messageID MessageID
	json      json.RawMessage
	sequence  uint64
}

func newEventRecord(messageID MessageID, raw json.RawMessage, sequence uint64) eventRecord {
	return eventRecord{
		messageID: messageID,
		json:      bytes.Clone(raw),
		sequence:  sequence,
	}
}

func (r eventRecord) snapshot() Event {
	return Event{
		MessageID: r.messageID,
		JSON:      bytes.Clone(r.json),
		Sequence:  r.sequence,
	}
}

type resultRecord struct {
	id      AsyncID
	what    string
	ok      bool
	details json.RawMessage
}

func newResultRecord(id AsyncID, what string, ok bool, details json.RawMessage) resultRecord {
	return resultRecord{
		id:      id,
		what:    what,
		ok:      ok,
		details: bytes.Clone(details),
	}
}

func (r resultRecord) snapshot() AsyncResult {
	return AsyncResult{
		ID:      r.id,
		What:    r.what,
		OK:      r.ok,
		Details: bytes.Clone(r.details),
	}
}
