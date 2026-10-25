package maa

// TaskID 标识一个已提交到实例的任务。
//
// raw 层对应 C 类型 AsstId = int32_t(保留符号位,不做无符号化解释)。
// 任务提交失败时原生返回 0,因此 0 不是合法的已提交任务 ID。
type TaskID int32

// AsyncID 标识一次异步提交(如连接、截图)及其最终结果的关联键。
//
// raw 层对应 C 的 int32_t 异步 ID(AsstId / async_call_id,保留符号位)。
// 0 是异步提交的失败值;非零 AsyncID 只证明提交成功,最终结果必须以
// 回调消息中的 async_call_id 关联确认,不能把提交成功当作执行成功。
type AsyncID int32

// MessageID 对应回调消息类型(AsstCallback 的 message 字段)。
//
// 与 TaskID、AsyncID 一样基于 C 的 int32_t,但语义不同:
// 它区分回调消息的种类,而非标识某次提交或某个任务。
type MessageID int32

// TaskID、AsyncID、MessageID 是三个互不混用的命名类型:
// 尽管底层类型都是 int32,Go 类型系统要求显式转换才能互换,
// 防止把异步 ID 当任务 ID、或把消息类型当标识符使用。
