# 参与开发

先阅读 [两人协作计划](docs/collaboration/README.md) 和 [GitHub 任务索引](docs/collaboration/github-index.md)，认领一个准备就绪的 Issue。当前产品定位仍是单人本地视频工具，共同开发不意味着引入多人在线编辑。

写文档、界面文案和提交信息前，先读 [中文技术文档写作规范](docs/writing-guide.md)，交付前按其中的自查清单核对。

## 本地准备

依赖 Go 1.24+ 和 FFmpeg/ffprobe 6+（libx264/AAC）；P1 验证环境见 [验收记录](docs/p1-acceptance.md)。

```sh
go build -o bin/video-agent ./cmd/video-agent
go test -race ./...
go vet ./...
VIDEO_AGENT_INTEGRATION=1 go test -count=1 -v ./integration
```

普通 `go test` 会跳过媒体验收。上面的显式媒体测试使用本地生成素材；公开影片样本测试需要另设 `VIDEO_AGENT_SAMPLE`，缺少样本会跳过该单项，不能宣称已经验收所有真实任务。

## 交付 PR

- 从最新 `main` 创建 `feat/<任务ID>-<描述>` 或 `fix/<任务ID>-<描述>` 分支；提交前确认没有混入其他任务的文件。
- 使用 PR 模板，关联 Issue、列出验收证据及配置/迁移影响。
- 另一位开发者负责评审；涉及共享模型、数据库和渲染语义时，先在 Issue 约定接口和文件修改范围。
- 合并后检查下游任务是否解除阻塞，更新状态标签。任务拆分细节和初始依赖见 [tasks.json](docs/collaboration/tasks.json)，实际进度以 GitHub 为准。

当前未配置自动 assignee、CODEOWNERS 或强制分支规则，因为第二位开发者账号尚未指定。协作约定先通过 Issue 和评审执行，后续再按实际成员配置。
