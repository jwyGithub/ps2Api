package api

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
)

// 本文件集中处理「Postman 原生工具 <-> Codex 客户端 exec custom 工具」的双向翻译,
// 从 Responses 适配层(responses.go)拆出,便于独立演进与测试。
//
// 背景:此前把 Postman 的 executeShellCommand/readFile 透传成 function_call、或翻译成
// Codex 内建 local_shell_call,都撞在同一堵墙——客户端逐条回 "unsupported call" 死循环。
// 核对三份 codex trace 后确认:当前客户端在 additional_tools(functions namespace)里声明的
// 执行入口是一个 type:"custom" 的工具 exec(lark grammar,input 是原始 JS 源码文本),真正的
// 能力挂在全局 tools 对象上的嵌套工具(tools.exec_command / tools.apply_patch ...)。
//
// 因此正确做法:把可映射的原生工具翻译成一次 exec custom tool 调用,input 写 JS 文本
// `const r = await tools.exec_command({cmd, workdir}); text(r...)`,客户端执行后回
// custom_tool_call_output。注意必须 text() 把返回值吐回去(isolate 无 console),否则输出为空。
// wire 格式(已对 OpenAI Responses API custom tools 规范核实):
//   出站 { type:"custom_tool_call", name:"exec", input:<JS文本>, call_id }
//   回程 { type:"custom_tool_call_output", call_id, output }

// codexExecForce 强制启用 exec custom tool 翻译(测试/兜底用),即使 tools 里没显式声明 exec。
// 正常情况下靠 codexExecDeclared 运行时探测客户端声明,不依赖 env。
var codexExecForce = os.Getenv("PS2API_CODEX_EXEC") != ""

// codexExecName 是客户端声明的 custom 执行工具名。
const codexExecName = "exec"

// codexExecDeclared 运行时探测:客户端是否声明了 type:"custom" 且 name=="exec" 的执行工具。
// 声明了才把原生工具翻译成 exec custom tool 调用;没声明就走原路(function_call),不影响其他客户端。
//
// 关键:实测 codex/ChatGPT 客户端并不把 exec 放在顶层 tools,而是塞进 input 里的一个
// {type:"additional_tools", role:"developer", tools:[{name:"functions", tools:[{type:"custom",name:"exec",...}]}]}
// 项(functions namespace 下再嵌套一层)。因此顶层 tools 探测不到,必须连 input 一起扫。
// 这里对 tools 做直接匹配,对 input 做有界递归扫描,命中任意 {type:"custom", name:"exec"} 声明即算声明。
func codexExecDeclared(tools []map[string]interface{}, input json.RawMessage) bool {
	for _, t := range tools {
		if execToolDecl(t) {
			return true
		}
	}
	if len(input) > 0 {
		var raw interface{}
		if json.Unmarshal(input, &raw) == nil && scanForExecDecl(raw) {
			return true
		}
	}
	return false
}

// execToolDecl 判断一个对象是否为 exec custom 工具「声明」({type:"custom", name:"exec"})。
// 注意:custom_tool_call 回显项 type 是 "custom_tool_call" 而非 "custom",不会误命中。
func execToolDecl(m map[string]interface{}) bool {
	if m == nil {
		return false
	}
	typ, _ := m["type"].(string)
	name, _ := m["name"].(string)
	return typ == "custom" && name == codexExecName
}

// scanForExecDecl 在任意解码后的 JSON 结构里有界递归查找 exec custom 工具声明。
// 客户端把 exec 嵌在 input→additional_tools→tools[]→tools[] 里,层级不固定,故做通用递归。
func scanForExecDecl(v interface{}) bool {
	switch t := v.(type) {
	case map[string]interface{}:
		if execToolDecl(t) {
			return true
		}
		for _, val := range t {
			if scanForExecDecl(val) {
				return true
			}
		}
	case []interface{}:
		for _, val := range t {
			if scanForExecDecl(val) {
				return true
			}
		}
	}
	return false
}

// execMappable 判断某个 Postman 原生工具能否翻译成 exec custom tool 调用。
// 这些工具 Codex 侧本无 handler(裸名转发必被判 "unsupported call" 死循环),
// 但都能用 tools.exec_command 跑一条 shell 命令等价实现。
func execMappable(name string) bool {
	switch name {
	case "executeShellCommand", "readFile":
		return true
	default:
		return false
	}
}

// execInputFor 按工具名把可映射的原生工具调用翻成 exec custom tool 的 input(原始 JS 文本)。
func execInputFor(name, argsJSON string) (string, bool) {
	switch name {
	case "executeShellCommand":
		return execShellInput(argsJSON)
	case "readFile":
		return execReadFileInput(argsJSON)
	default:
		return "", false
	}
}

// execCommandJS 把一条 shell 命令 + 可选工作目录包成 exec 的 JS 输入文本。
// 用 JSON 编码参数对象(JSON 是 JS 对象字面量子集),避免手写引号被 cmd 里的特殊字符破坏。
//
// 关键:exec isolate「no console」,exec_command 的返回值若不显式经全局 text() 追加到
// Output,模型只会收到空输出——实测表现为客户端 custom_tool_call_output 恒为
// "Script completed / Wall time / Output:\n"(Output 段为空),模型据此误判「无文件/shell
// 权限」而放弃执行。因此必须捕获返回值并 text() 出来。返回可能是字符串或对象,优先取
// .output 字段,退化为整体(text() 对非字符串做 JSON.stringify)。
func execCommandJS(cmd, workdir string) string {
	obj := map[string]interface{}{"cmd": cmd}
	if workdir != "" {
		obj["workdir"] = workdir
	}
	b, _ := json.Marshal(obj)
	return "const r = await tools.exec_command(" + string(b) + "); text(r && r.output !== undefined ? r.output : r)"
}

// execShellInput 把 executeShellCommand 的参数({projectPath, command})翻成 exec JS 输入。
func execShellInput(argsJSON string) (string, bool) {
	var a struct {
		ProjectPath string `json:"projectPath"`
		Command     string `json:"command"`
	}
	if json.Unmarshal([]byte(argsJSON), &a) != nil || a.Command == "" {
		return "", false
	}
	return execCommandJS(a.Command, a.ProjectPath), true
}

// execReadFileInput 把 readFile({filePath, limit, offset}) 翻成读取文件的只读 shell 命令,
// 再包成 exec JS 输入:带 offset+limit 用 sed 取行区间、仅 limit 用 head、否则 cat 全量。
// offset 为 1-based 起始行,limit 为读取行数(与上游 readFile 语义一致)。
func execReadFileInput(argsJSON string) (string, bool) {
	var a struct {
		FilePath string `json:"filePath"`
		Path     string `json:"path"`
		Limit    int    `json:"limit"`
		Offset   int    `json:"offset"`
	}
	if json.Unmarshal([]byte(argsJSON), &a) != nil {
		return "", false
	}
	path := a.FilePath
	if path == "" {
		path = a.Path
	}
	if path == "" {
		return "", false
	}
	q := shellSingleQuote(path)
	var cmd string
	switch {
	case a.Offset > 1 && a.Limit > 0:
		cmd = fmt.Sprintf("sed -n '%d,%dp' %s", a.Offset, a.Offset+a.Limit-1, q)
	case a.Limit > 0:
		cmd = fmt.Sprintf("head -n %d %s", a.Limit, q)
	default:
		cmd = "cat " + q
	}
	return execCommandJS(cmd, ""), true
}

// shellSingleQuote 用单引号安全包裹路径,内部单引号按 '\'' 转义,防止路径里的空格/特殊字符
// 破坏 shell 命令行。
func shellSingleQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// execInputToArgs 把回显的 exec custom_tool_call.input(JS 文本)best-effort 还原成
// executeShellCommand 参数,供入站历史重建(内部管道只认 executeShellCommand)。
// 从 `await tools.exec_command({...})` 里抠出 JSON 对象取 cmd/workdir;抠不出就整段当 command。
// 精确度不影响续期——nativeToolResponse 靠 call_id→groupID 闭环,LookupConversation 在
// assistant 消息之前的前缀即可命中。
func execInputToArgs(input string) string {
	if i := strings.Index(input, "{"); i >= 0 {
		if j := strings.LastIndex(input, "}"); j > i {
			var obj struct {
				Cmd     string `json:"cmd"`
				Workdir string `json:"workdir"`
			}
			if json.Unmarshal([]byte(input[i:j+1]), &obj) == nil && obj.Cmd != "" {
				out := map[string]interface{}{"command": obj.Cmd}
				if obj.Workdir != "" {
					out["projectPath"] = obj.Workdir
				}
				b, _ := json.Marshal(out)
				return string(b)
			}
		}
	}
	b, _ := json.Marshal(map[string]interface{}{"command": input})
	return string(b)
}
