# 合成浏览器媒体样本

仅用于隔离实例测试，不来自真实素材。PNG / MP4 是 FFmpeg testsrc2 生成的 160×90 测试图，视频长 1 秒；WAV 是 1 秒 440 Hz 正弦波。测试样本可按 CC0-1.0 使用，不包含任何第三方作品。

```sh
ffmpeg -f lavfi -i 'testsrc2=size=160x90:rate=10' -t 1 -an -c:v libx264 -pix_fmt yuv420p -movflags +faststart -map_metadata -1 sample.mp4
ffmpeg -f lavfi -i 'sine=frequency=440:sample_rate=8000:duration=1' -c:a pcm_s16le -map_metadata -1 sample.wav
ffmpeg -f lavfi -i 'testsrc2=size=160x90:rate=1' -frames:v 1 -threads 1 -map_metadata -1 sample.png
```

生成不属于测试运行前置；以上小文件已固定在仓库。文本、代码、配置和错误样本由 Go 测试夹具生成。
