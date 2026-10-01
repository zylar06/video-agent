# Video Agent

一个能听懂人话的长视频理解与智能剪辑助手：把长视频解析成多维、可检索且可追溯的证据，让创作者用自然语言精确控制剪辑结果。用户在浏览器中导入素材、分析内容、审阅有来源的选片方案，确认后预览并导出 MP4。素材、工程和渲染产物保留在本地；仅当用户配置模型 provider 时，提取出的音频片段和代表帧会发送到该 provider 用于分析。

当前主闭环是：导入长视频或字幕 → 分析为带时间范围的证据 → 自然语言提出主题与时长目标 → 审阅方案 → 确认、预览与导出。底层的版本化时间线、SQLite、FFmpeg 和异步任务为这个闭环提供可靠实现。

## 项目状态

### 已完成

- 可靠剪辑底座：多素材导入、素材哈希、版本化时间线、裁剪/替换/插入/删除/重排、锁定、撤销与恢复。
- FFmpeg 预览和 MP4 导出：统一帧率与画幅，完成分辨率、帧数、时长、编码和完整解码校验后才发布成片。
- 异步渲染任务：任务提交、状态查询、进度、取消、失败状态和任务列表。
- 内部工具接口：统一 v1 JSON envelope、证据写入、搜索、proposal、编辑提交、冲突处理和版本恢复，供 Web 与自动化回归复用。
- 本地 HTTP 服务：loopback 服务、受控 MP4 产物读取、HTTP Range 播放和路径穿越防护。
- Web 主入口：浏览器中创建项目、导入本地视频和字幕、执行分析、自然语言选片、确认方案、预览与导出，不需要手写 JSON。
- Docker/Windows 部署：Dockerfile、Compose 和 `video-agent.bat`，Go/FFmpeg/ffprobe 一起打包。
- 真实素材回放：使用指定 B 站视频完成导入、证据检索、连续 5 次编辑、冲突恢复、异步导出和取消验证。
- 证据分析：支持 SRT/VTT 导入、可选 OpenAI-compatible ASR/视觉 provider、最多 48 张代表帧、SQLite 分析运行缓存，以及带来源时间戳的检索/proposal。
- 自动化验证：Go 单元测试、race、vet、FFmpeg 端到端测试、内部 API 测试和 Docker 烟测。
- 分层草稿编辑：对话生成草稿方案，每个候选片段带来源范围；用户可逐条修改（裁剪、删除、调整顺序和时间）、查看版本、确认后写入时间线，或从任一新版本恢复。

### 未完成

- 高级理解：场景切分、embedding/情绪与节奏评分，以及针对复杂创作目标的质量验收。
- 模型 provider 需要用户配置 API；没有 provider 时仍可用本地字幕和代表帧完成可追溯闭环。
- 精确对话控制：草稿接口已支持逐条修改和版本恢复，Web 仍是对话式草稿界面；可视化时间线编辑器、片段级拖拽和成片帧预览尚未提供。
- 成片能力：中文字幕渲染、BGM/混音、9:16 fit/crop、复杂构图和更细的风格控制。
- 可靠性：服务重启后的任务恢复、自动重试、临时文件回收和工程目录迁移后的路径修复。

当前边界：这是一个聚焦“证据理解 → 自然语言控制 → 用户确认 → 预览/导出”的本地助手，不是剪映替代品。字幕渲染、BGM、9:16 构图和复杂风格控制仍在后续范围。

## 产品定义

这是一个面向创作者的本地长视频理解与智能剪辑产品，而不是通用 Code Agent 或剪映替代品。自然语言负责表达创作目标；多维 Evidence、结构化时间线、版本化编辑和可验证渲染负责让剪辑结果可追溯、可控制。完整定位见 [产品定义](docs/product-definition.md)、[需求](docs/requirements.md) 与 [路线图](docs/roadmap.md)。

## 现在有什么

- 项目与多素材导入：ffprobe 元数据、SHA256 去重、本地素材快照。
- SQLite 保存全部时间线版本：裁剪、替换、删除、插入、重排、锁定、解锁、撤销与恢复。
- 原子版本提交：拒绝旧 revision；重复 operation ID 同载荷重放、不同载荷报错。
- 多源渲染：统一帧率和尺寸，处理横竖屏、旋转、原声、单/双声道及无声素材。
- 低分辨率预览、真实 H.264/AAC MP4、持久化任务及逐片段来源记录。
- 验证分辨率、帧数、时长和完整解码后才发布文件；不覆盖已有导出。
- 内部 JSON CLI 与工具入口；证据检索、proposal、异步任务和 HTTP API 详见 [内部接口](docs/api.md)。

## 运行

需要 Go 1.24+、FFmpeg/ffprobe 6+（含 libx264 和 AAC 编码器）：

```sh
go build -o bin/video-agent ./cmd/video-agent
bin/video-agent --help
go test -race ./...
VIDEO_AGENT_INTEGRATION=1 VIDEO_AGENT_ACCEPTANCE_DIR="$PWD/data/p1-acceptance" go test -count=1 -v ./integration
```

最后一个命令生成真实测试视频，经 CLI 完成 36 秒成片、预览、重排、替换和恢复；输出目录由测试日志显示，包含 MP4、版本历史和 JSON 验收报告。没有 FFmpeg 时会明确失败。普通 `go test` 不执行媒体验收。

创作者请优先启动下方 Web 页面；[工程使用说明](docs/p1-usage.md) 和 [验收记录](docs/p1-acceptance.md) 仅说明底层 CLI 与历史验证范围。

## Docker 与 Windows

Docker 版本把 Go、FFmpeg 和 ffprobe 打进镜像；Windows 10/11 用户只需安装并启动 Docker Desktop 的 Linux containers 模式，不需要单独装 Go 或 FFmpeg。素材、数据库和导出文件会留在仓库的 `workspace/`，不会消失在容器内。

PowerShell 中执行：

```powershell
git clone https://github.com/zylar06/video-agent.git
cd video-agent
.\video-agent.bat --help
```

首次调用会自动构建镜像。把视频放入 `workspace\input\`，再使用容器内路径执行命令：

```powershell
.\video-agent.bat project create --id demo --name "我的项目"
.\video-agent.bat assets import --project demo --path /workspace/input/video-1.mp4
```

也可以在 macOS/Linux 使用相同 Compose 配置：

```sh
docker compose run --rm --build video-agent --data /workspace/data --help
```

完整的 Windows 操作、时间线 JSON 例子、备份和排错见 [Docker 使用说明](docs/docker.md)。启动 Docker 中的 Web 服务时使用：

```sh
docker compose run --rm --service-ports video-agent --data /workspace/data serve --addr 0.0.0.0:8090
```

容器内的 `0.0.0.0` 仅用于映射到 Docker 的 loopback 端口；二进制直接运行时 `serve` 只允许 loopback。内部工具接口与历史自动化流程见 [接口说明](docs/api.md) 和 [自动化调用流程](docs/agent-workflow.md)。

## 目录

```text
cmd/video-agent/       CLI 入口
internal/domain/       素材、片段、时间线和编辑操作
internal/app/          项目服务与渲染任务入口
internal/store/        SQLite 项目、资产、历史版本和任务
internal/edit/         版本校验、编辑补丁和幂等提交
internal/catalog/      分析证据与语义检索接口
internal/media/        本地文件、内容哈希、导入、ffprobe 和子进程
internal/render/       渲染计划、FFmpeg 编译和产物验证
internal/agent/        工具契约和注册表
docs/                  产品、架构和实施计划
examples/              时间线与编辑操作 JSON 样例
integration/           真实媒体和 CLI 端到端验收
```

## 下一步

下一阶段聚焦精确的局部对话编辑、场景与语义理解、字幕/BGM/竖屏成片能力，以及任务恢复。模型 provider、场景理解与成片质量会持续迭代。

启动 Web 页面：

```sh
bin/video-agent --data data/demo serve --addr 127.0.0.1:8090
```

然后打开 <http://127.0.0.1:8090/>。在页面中通过文件选择或拖放导入素材；不需要填写本机路径。Docker 会将上传内容保存到挂载的 workspace 数据目录。

现在可以直接在页面中创建项目，并通过文件选择或拖放导入 MP4/MOV/M4V；不必填写路径、素材 ID 或 JSON。上传 SRT/VTT 后可离线分析字幕。若要自动转写，请只在本机环境文件中配置百炼兼容变量（切勿提交密钥）：

```sh
AUTOCLIP_ASR_BASE_URL=https://<workspace>.cn-beijing.maas.aliyuncs.com/compatible-mode/v1
AUTOCLIP_ASR_MODEL=qwen-audio-3.1-asr-flash
AUTOCLIP_ASR_API_KEY=<rotated-local-key>
AUTOCLIP_TEXT_MODEL=qwen-plus
AUTOCLIP_VISION_MODEL=qwen-vl-plus
```

`qwen-audio-3.1-asr-flash` 由服务端将音频分成不超过四分钟的片段提交，返回逐句时间戳；服务会使用百炼原生多模态端点，而不是 OpenAI-compatible 转写端点。API Key 不会暴露给浏览器。启用 ASR 或视觉分析即表示允许服务端将相应的音频片段或代表帧发送到你配置的 provider。文本和视觉模型分别通过已有的 `AUTOCLIP_TEXT_*` 与 `AUTOCLIP_VISION_*` 读取。

## 两人协作开发

任务已按 A（媒体与智能）/ B（平台与交互）拆分，包含依赖、模块边界和验收条件。先看 [协作计划](docs/collaboration/README.md)、[GitHub 任务索引](docs/collaboration/github-index.md) 和 [贡献指南](CONTRIBUTING.md)，再认领 Issue。产品仍面向单人本地使用。

完整范围见 [产品定义](docs/product-definition.md)、[需求](docs/requirements.md)、[架构](docs/architecture.md)、[路线图](docs/roadmap.md)、[调研结论](docs/research.md) 和 [验证说明](docs/verification.md)。
