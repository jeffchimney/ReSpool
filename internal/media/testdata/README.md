# Test fixtures

Tiny sine-wave files generated with ffmpeg 8. To regenerate them, write the
chapter metadata below to `meta.txt` and run the commands from this folder.

```text
;FFMETADATA1
title=Chaptered Book
album=Chaptered Book
artist=Ada Author
description=A tiny book used in tests.

[CHAPTER]
TIMEBASE=1/1000
START=0
END=2000
title=Opening

[CHAPTER]
TIMEBASE=1/1000
START=2000
END=4500
title=The Middle

[CHAPTER]
TIMEBASE=1/1000
START=4500
END=6000
title=Ending
```

```sh
ffmpeg -f lavfi -i color=c=0x3366aa:s=64x64 -frames:v 1 cover.jpg

# ID3 tags and embedded cover art, CBR with an Info header
ffmpeg -f lavfi -i sine=frequency=440:duration=3 -i cover.jpg -map 0:a -map 1:v -c:v copy \
  -ac 1 -ar 22050 -c:a libmp3lame -b:a 32k \
  -metadata title="Chapter One" -metadata album="Test Book" -metadata artist="Test Author" \
  -metadata album_artist="Test Author" -metadata track=1 \
  -metadata:s:v title="Album cover" -metadata:s:v comment="Cover (front)" -id3v2_version 3 tagged.mp3

# VBR with a Xing header
ffmpeg -f lavfi -i sine=frequency=330:duration=4 -ac 1 -ar 22050 -c:a libmp3lame -q:a 9 vbr.mp3

# CBR without a Xing header (duration is estimated from the bitrate)
ffmpeg -f lavfi -i sine=frequency=550:duration=5 -ac 1 -ar 22050 -c:a libmp3lame -b:a 32k -write_xing 0 cbr-noxing.mp3

# Nero chpl box and QuickTime chapter track
ffmpeg -f lavfi -i sine=frequency=440:duration=6 -i meta.txt -map 0:a -map_metadata 1 -map_chapters 1 \
  -ac 1 -ar 22050 -c:a aac -b:a 24k -f ipod chapters.m4b

# QuickTime chapter track only
ffmpeg -f lavfi -i sine=frequency=440:duration=6 -i meta.txt -map 0:a -map_metadata 1 -map_chapters 1 \
  -ac 1 -ar 22050 -c:a aac -b:a 24k -movflags +disable_chpl -f ipod qt-chapters.m4b

ffmpeg -f lavfi -i sine=frequency=440:duration=2 -ac 1 -ar 8000 tone.flac
```
