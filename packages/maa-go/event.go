// 本文件定义公开输出数据 Event 与 AsyncResult,以及内部记录/缓存
// 持有类型与其深拷贝快照辅助(绑定设计 §5、§6.1)。每个公开输出
// 拥有可安全保留的 Go 数据:Event 与 AsyncResult 不共享可由用户
// 修改的内部 JSON 缓冲区;重复或并发取得快照各自得到独立副本,
// 修改先前副本(包括原地改写 RawMessage 底层字节)不改变内部缓存、
// 其他副本或之后的任何快照;实例关闭后这些副本仍有效。
//
// 原始 JSON 不经 map[string]any 的 float64 往返:记录构造时逐字节
// 复制原文,未知任务类型、未知字段、未知消息 ID 与超过 2^53 的
// 整数按原文字节保留;JSON 透传不承诺空白和转义形式逐字节不变,
// 但不能丢字段、经浮点转换改变数值或截断有效内容。对关联字段
// (如 AsyncCallInfo 的必要字段)的精确解析由后续接线任务按
// json.Number 另行完成,本文件不做任何解析。
//
// 本单元只定义数据与复制点:callback 准入、事件分发器、Operation
// 缓存与 Wait 接线由 M3 后续任务承接。记录一经构造即不可变,并发
// 读取(含并发快照)安全;跨调用的写同步由持有记录的分发器/缓存
// 的锁负责,记录类型自身不加锁。

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

// eventRecord 是事件队列与内部缓存持有的事件记录:拥有独立的
// RawMessage 缓冲区,构造时逐字节 clone 来源原文,与调用者或
// callback 借用视图的缓冲区不共享底层数组。
type eventRecord struct {
	messageID MessageID
	json      json.RawMessage
	sequence  uint64
}

// newEventRecord 构造事件记录:立即逐字节复制 raw,此后调用者对
// 原缓冲区的任何修改(包括原地改写)都不影响记录。
func newEventRecord(messageID MessageID, raw json.RawMessage, sequence uint64) eventRecord {
	return eventRecord{
		messageID: messageID,
		json:      bytes.Clone(raw),
		sequence:  sequence,
	}
}

// snapshot 返回可安全交给调用者的独立深拷贝 Event:修改返回副本
// (包括原地改写 JSON 底层字节)不影响记录本身,也不影响之后的
// 任何快照。
func (r eventRecord) snapshot() Event {
	return Event{
		MessageID: r.messageID,
		JSON:      bytes.Clone(r.json),
		Sequence:  r.sequence,
	}
}

// resultRecord 是 Operation 内部缓存持有的异步结果记录:拥有独立
// 的 details RawMessage,构造时逐字节 clone 来源原文,与公开副本
// 及其他缓存不共享底层数组。what 为只读 Go string,按语言语义共享。
type resultRecord struct {
	id      AsyncID
	what    string
	ok      bool
	details json.RawMessage
}

// newResultRecord 构造结果记录:立即逐字节复制 details,此后调用者
// 对原缓冲区的任何修改都不影响记录。
func newResultRecord(id AsyncID, what string, ok bool, details json.RawMessage) resultRecord {
	return resultRecord{
		id:      id,
		what:    what,
		ok:      ok,
		details: bytes.Clone(details),
	}
}

// snapshot 返回可安全交给调用者的独立深拷贝 AsyncResult:重复或
// 并发 Wait 各取得独立副本,修改先前副本不改变缓存结果或其他
// Wait 的返回。
func (r resultRecord) snapshot() AsyncResult {
	return AsyncResult{
		ID:      r.id,
		What:    r.what,
		OK:      r.ok,
		Details: bytes.Clone(r.details),
	}
}
