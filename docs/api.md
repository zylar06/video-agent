# 内部本地服务接口（v1）

服务为 Web 产品提供本机受限接口，也供 CLI、调试和自动化回归复用。它不是面向第三方的公共 API，也不会给予任意 shell、数据库或文件系统权限。创作者应使用浏览器中的自然语言入口；下列接口是实现细节，可能随产品演进调整。

启动服务（默认只监听 loopback）：

```sh
bin/video-agent --data data/demo serve --addr 127.0.0.1:8090
```

所有工具结果都是下列 envelope；业务代码应使用 `error.code`，不要按错误文案分支。

```json
{"api_version":"v1","ok":true,"result":{}}
```

错误码：`invalid_request`、`not_found`、`no_match`、`model_unavailable`、`timeout`、`cancelled`、`revision_conflict`、`internal`。

## 入口

| 方法 | 路径 | 说明 |
|---|---|---|
| `GET` | `/v1/health` | 本地服务健康状态 |
| `POST` | `/v1/ui/projects` | Web 创建项目 |
| `GET` / `POST` | `/v1/ui/projects/{id}/assets` | Web 查询或上传素材与可选字幕 |
| `POST` | `/v1/ui/analyze` | Web 发起或复用证据分析 |
| `POST` | `/v1/ui/proposals` | Web 将自然语言目标转为可审阅方案 |
| `POST` | `/v1/ui/proposals/confirm` | Web 确认方案并保存时间线 |
| `POST` | `/v1/chat` | 将自然语言剪辑请求解析为意图，并返回有来源候选 |
| `GET` | `/v1/tools` | 可调用工具名称 |
| `POST` | `/v1/tools/{tool}` | 调用工具，body 是 JSON 输入 |
| `GET` | `/v1/jobs/{id}` | 读取异步导出任务 |
| `POST` | `/v1/jobs/{id}/cancel` | 取消 queued/running 导出 |
| `GET` | `/v1/artifacts/{job_id}` | 读取成功任务的 MP4，支持 HTTP Range |

产物读取只允许数据库中已完成任务登记、并位于数据目录 exports 下的常规文件；路径穿越和 CLI 任意输出位置不会被 API 暴露。

## 工具

项目、素材和时间线工具直接映射剪辑领域对象，继续执行资产归属、revision、锁定和幂等操作 ID 校验。

证据与渲染工具：

| 工具 | 输入重点 | 结果 |
|---|---|---|
| `evidence_add` | 完整 `Evidence`（项目、素材、源微秒范围及字幕/视觉内容） | 持久化、带素材内容哈希的来源证据 |
| `analyze` | `project_id`、`asset_id`、可选 `subtitle_path`、`visual`、`provider`、`parameters` | 执行/复用分析运行，返回 `run`（阶段状态、缓存键）和带来源的 `evidence`；可用本地 SRT/VTT，无字幕且未配置 ASR 时返回 `model_unavailable` |
| `search` | `project_id`、`query`、可选 `asset_ids`、`limit` | 稳定排序的证据命中；无命中为 `no_match` |
| `proposal_create` | `timeline_id`、`query`、`limit` | 未提交的 `EditProposal`，含连续 revision 的插入操作 |
| `render_submit` | `timeline_id`、可选 `revision`、`preview`、`filename` | 立即返回 `queued` job；文件名只能是 `.mp4` 基名 |
| `jobs_get`、`jobs_list`、`jobs_cancel` | 任务 ID（list 无输入） | 查询、列表或取消任务 |

分析使用可选 provider。ASR 支持 `VIDEO_AGENT_ASR_*` 或 `AUTOCLIP_ASR_*`；配置 `VIDEO_AGENT_VISION_*` 或 `AUTOCLIP_VISION_*` 启用全片 1fps 的分批画面描述；`AUTOCLIP_TEXT_*` 提供自然语言理解与方案解释，不会被误当成 ASR。`qwen-audio-*` 使用百炼原生多模态端点和 SSE 逐句时间戳；`qwen3-asr-flash` 仍经 chat completions 接收分段音频，其他兼容 ASR 可经 audio transcriptions 接收音频。provider 失败会持久化为 failed，不会伪装成完成。

对话请求示例：

```json
{"project_id":"demo","asset_id":"asset-1","message":"剪成 1 分钟高能集锦","subtitle_path":"/workspace/input/video.srt","visual":true}
```

本地字幕与代表帧示例：

```sh
printf '%s\n' '{"project_id":"demo","asset_id":"asset-1","subtitle_path":"/workspace/input/video.srt","visual":true}' \
  | bin/video-agent --data data/demo tool call --tool analyze --file -
```

## JSON CLI

CLI 用同一服务而非另一套业务规则：

```sh
bin/video-agent --data data/demo tool list
printf '%s\n' '{"id":"demo"}' | bin/video-agent --data data/demo tool call --tool project_get --file -
```

`tool call` 的成功与失败都写出 v1 envelope；失败退出码非零。异步任务需要运行中的 `serve` 进程来执行，避免 CLI 进程退出后无人消费队列。
