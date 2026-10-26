// 本文件是 Event/AsyncResult 数据快照所有权的用例(MAA-14):纯
// Go 验证,不依赖 MaaCore、设备或业务任务模型。验证记录构造时的
// 逐字节复制、快照副本与内部缓存及先前副本的独立性,以及并发快照
// 的安全性与独立性(-race 下运行)。

package maa

import (
	"encoding/json"
	"strings"
	"sync"
	"testing"
)

// 事件与结果的测试原文:含未知字段与超过 2^53 的整数,验证逐字节
// 保留,不经 float64 往返。
const (
	testEventJSON     = `{"what":"RunningTask","unknown":{"big":9007199254740993}}`
	testResultWhat    = "ConnectReady"
	testResultDetails = `{"async_call_id":9,"extra":{"big":9007199254740993}}`
)

// TestEventRecordConstructionCopiesInput 验证:构造记录后改写来源
// 缓冲区(callback 借用视图按契约只在本次调用有效,来源可被复用或
// 改写),记录持有的原文不受影响。
func TestEventRecordConstructionCopiesInput(t *testing.T) {
	src := make([]byte, len(testEventJSON))
	copy(src, testEventJSON)
	rec := newEventRecord(MessageID(7), src, 3)

	for i := range src {
		src[i] = 'X'
	}

	if got := string(rec.json); got != testEventJSON {
		t.Fatalf("记录原文被来源改写: got %q, want %q", got, testEventJSON)
	}
	if rec.messageID != 7 || rec.sequence != 3 {
		t.Errorf("记录元数据不符: messageID=%d sequence=%d", rec.messageID, rec.sequence)
	}
}

// TestEventRecordSnapshotIsIndependent 验证:每次快照返回独立副本,
// 原地改写先前副本(包括 JSON 底层字节)不影响内部缓存,也不影响
// 下一次快照。
func TestEventRecordSnapshotIsIndependent(t *testing.T) {
	rec := newEventRecord(MessageID(7), json.RawMessage(testEventJSON), 3)

	s1 := rec.snapshot()
	if s1.MessageID != 7 || s1.Sequence != 3 || string(s1.JSON) != testEventJSON {
		t.Fatalf("第一次快照不符: %+v", s1)
	}

	// 原地改写快照副本的底层字节。
	for i := range s1.JSON {
		s1.JSON[i] = '!'
	}
	s1.MessageID = 99
	s1.Sequence = 42

	s2 := rec.snapshot()
	if s2.MessageID != 7 || s2.Sequence != 3 || string(s2.JSON) != testEventJSON {
		t.Errorf("改写先前快照影响了下一次快照: %+v", s2)
	}
	if got := string(rec.json); got != testEventJSON {
		t.Errorf("改写快照改写了内部缓存: %q", got)
	}
	// 先前副本保持被改写状态:它是调用者拥有的独立数据。
	if s1.MessageID != 99 || string(s1.JSON) != strings.Repeat("!", len(testEventJSON)) {
		t.Errorf("先前副本的修改丢失: %+v %q", s1.MessageID, string(s1.JSON))
	}
}

// TestAsyncResultRecordSnapshotIsIndependent 验证:结果记录构造时
// 逐字节复制 details;每次快照返回独立副本,改写先前副本(包括
// details 底层字节)不影响内部缓存与下一次快照;ID/What/OK 原样
// 保留,OK=false 不被伪造或解释。
func TestAsyncResultRecordSnapshotIsIndependent(t *testing.T) {
	src := make([]byte, len(testResultDetails))
	copy(src, testResultDetails)
	rec := newResultRecord(AsyncID(9), testResultWhat, false, src)

	for i := range src {
		src[i] = 'X'
	}

	s1 := rec.snapshot()
	if s1.ID != 9 || s1.What != testResultWhat || s1.OK {
		t.Fatalf("第一次快照元数据不符: %+v", s1)
	}
	if string(s1.Details) != testResultDetails {
		t.Fatalf("details 被来源改写: %q", string(s1.Details))
	}

	for i := range s1.Details {
		s1.Details[i] = '?'
	}
	s1.What = "tampered"
	s1.OK = true

	s2 := rec.snapshot()
	if s2.ID != 9 || s2.What != testResultWhat || s2.OK {
		t.Errorf("改写先前快照影响了下一次快照: %+v", s2)
	}
	if string(s2.Details) != testResultDetails {
		t.Errorf("下一次快照的 details 被改写: %q", string(s2.Details))
	}
	if got := string(rec.details); got != testResultDetails {
		t.Errorf("改写快照改写了内部缓存: %q", got)
	}
}

// TestEventAndResultSnapshotsDoNotShareBuffers 验证:Event 与
// AsyncResult 不共享可被用户修改的内部 JSON 缓冲区——两份快照的
// RawMessage 底层不重叠,互不串扰。
func TestEventAndResultSnapshotsDoNotShareBuffers(t *testing.T) {
	// 同一来源原文分别喂给两条记录:各自独立 clone,不共享。
	src := json.RawMessage(testEventJSON)
	evRec := newEventRecord(MessageID(1), src, 1)
	resRec := newResultRecord(AsyncID(2), testResultWhat, true, src)

	ev := evRec.snapshot()
	res := resRec.snapshot()

	for i := range ev.JSON {
		ev.JSON[i] = '#'
	}
	if string(res.Details) != testEventJSON {
		t.Errorf("Event 快照的改写串扰到 AsyncResult 快照: %q", string(res.Details))
	}
	if got := string(evRec.snapshot().JSON); got != testEventJSON {
		t.Errorf("Event 快照的改写串扰到事件缓存: %q", got)
	}
	if got := string(resRec.snapshot().Details); got != testEventJSON {
		t.Errorf("Event 快照的改写串扰到结果缓存: %q", got)
	}
}

// TestConcurrentSnapshotsStayIndependent 验证:并发对同一记录取得
// 快照(模拟重复/并发 Wait 与事件投递),各 goroutine 改写自己的
// 副本互不影响,内部缓存与并发结束后的新快照保持原值。
func TestConcurrentSnapshotsStayIndependent(t *testing.T) {
	evRec := newEventRecord(MessageID(5), json.RawMessage(testEventJSON), 1)
	resRec := newResultRecord(AsyncID(6), testResultWhat, false, json.RawMessage(testResultDetails))

	const n = 8
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ev := evRec.snapshot()
			res := resRec.snapshot()
			if string(ev.JSON) != testEventJSON || string(res.Details) != testResultDetails {
				t.Errorf("并发快照取得被污染的副本: %q / %q", string(ev.JSON), string(res.Details))
				return
			}
			for j := range ev.JSON {
				ev.JSON[j] = 'E'
			}
			for j := range res.Details {
				res.Details[j] = 'R'
			}
		}()
	}
	wg.Wait()

	if got := string(evRec.snapshot().JSON); got != testEventJSON {
		t.Errorf("并发改写影响了事件缓存: %q", got)
	}
	if got := string(resRec.snapshot().Details); got != testResultDetails {
		t.Errorf("并发改写影响了结果缓存: %q", got)
	}
}
