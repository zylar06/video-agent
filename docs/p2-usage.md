# 证据分析使用说明

分析会把视频变成可核对的 `Evidence`：字幕 cue 带源时间戳，代表帧带源区间和本地 JPEG 路径，分析运行记录阶段状态与缓存键。它不是替用户猜测剪辑结果的黑盒；没有来源就不会生成片段。

## 本地字幕 + 代表帧

先导入项目和素材，然后调用 `analyze`：

```sh
bin/video-agent --data data/demo project create --id demo --name demo
bin/video-agent --data data/demo assets import --project demo --path /workspace/input/video.mp4
printf '%s\n' '{"project_id":"demo","asset_id":"<asset-id>","subtitle_path":"/workspace/input/video.srt","visual":true}' \
  | bin/video-agent --data data/demo tool call --tool analyze --file -
```

SRT 和 VTT 均支持。重复调用会按素材内容哈希、分析器版本、provider、参数、字幕内容哈希和视觉开关复用已完成运行。

## OpenAI-compatible provider

ASR：`VIDEO_AGENT_ASR_*` 或 `AUTOCLIP_ASR_*`。视觉：`VIDEO_AGENT_VISION_*` 或 `AUTOCLIP_VISION_*`。Base URL 可填服务根地址或带 `/v1` 的地址。`qwen-audio-*` 使用百炼原生多模态端点，服务端把音频切成不超过四分钟的片段并读取逐句 SSE 时间戳；qwen3-asr-flash 使用 `/chat/completions`，其他兼容 ASR 可以使用 `/audio/transcriptions`。视觉使用 `/chat/completions`，以 8 帧为一组分析全片 1fps 采样帧。

`AUTOCLIP_TEXT_*` 是文本理解与方案模型，不是音频转写接口，不能直接当 ASR 使用；没有独立 ASR 时请导入已有 SRT/VTT，或配置 ASR 变量。模型密钥只从本机环境变量读取，不会进入浏览器、数据库、日志或 Git。

provider 是可选的；没有密钥时，字幕导入和本地代表帧仍可用。provider 错误会保存为 failed 运行，修复配置后用相同请求重试即可。

## 在产品中使用

创作者在 Web 页面选择视频和可选字幕，执行分析后输入自然语言目标，例如“保留结论，剪成一分钟”。系统先检索带来源的 Evidence，再展示候选片段和方案；用户确认后才创建时间线、预览或导出。CLI 工具保留给调试与回归，所有片段仍必须能回溯到 Evidence 的源时间范围。
