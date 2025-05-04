# podcast-transcriber
A podcast transcriber

```
curl -X POST \
  -F "audio=@./test/NPR8115733396.mp3" \
  -F "description=< ./test/description.txt" \
  -F "original_transcript=< ./test/original_transcript.txt" \
  http://localhost:8080/upload-podcast
```