#!/bin/sh
# Generates a small fake audiobook library for trying ReSpool out, using
# ffmpeg sine tones. Usage: scripts/sample-library.sh [folder]
set -eu

out=${1:-sample-media}
mkdir -p "$out"
cd "$out"

ff() { ffmpeg -hide_banner -loglevel error -y "$@"; }
tone() { # tone <seconds> <frequency> <output> [ffmpeg options…]
	secs=$1 freq=$2 dst=$3
	shift 3
	ff -f lavfi -i "sine=frequency=$freq:duration=$secs" -ac 1 -ar 22050 "$@" "$dst"
}
cover() { # cover <color> <color> <output>
	ff -f lavfi -i "gradients=s=600x600:c0=$1:c1=$2:duration=1" -frames:v 1 "$3"
}

# A single m4b with chapters, an author folder and a cover.jpg.
mkdir -p "Ada Lovelace/Notes on the Engine"
cat > chapters.txt <<'EOF'
;FFMETADATA1
title=Notes on the Engine
album=Notes on the Engine
artist=Ada Lovelace
description=A short sample book with three chapters.

[CHAPTER]
TIMEBASE=1/1000
START=0
END=20000
title=The Analytical Engine

[CHAPTER]
TIMEBASE=1/1000
START=20000
END=40000
title=Note G

[CHAPTER]
TIMEBASE=1/1000
START=40000
END=60000
title=Afterword
EOF
ff -f lavfi -i "sine=frequency=392:duration=60" -i chapters.txt -map 0:a -map_metadata 1 -map_chapters 1 \
	-ac 1 -ar 22050 -c:a aac -b:a 32k -f ipod "Ada Lovelace/Notes on the Engine/Notes on the Engine.m4b"
rm chapters.txt
cover 0x264653 0x2a9d8f "Ada Lovelace/Notes on the Engine/cover.jpg"

# Tagged MP3s split across disc folders, with a description file.
mkdir -p "The Long Walk/CD1" "The Long Walk/CD2"
n=0
for part in "CD1/01 Departure" "CD1/02 The Ridge" "CD2/01 Rain" "CD2/02 Arrival"; do
	n=$((n + 1))
	disc=${part%%/*} # CD1
	name=${part#*/}  # 01 Departure
	tone 30 $((300 + n * 40)) "The Long Walk/$part.mp3" -c:a libmp3lame -b:a 32k \
		-metadata title="${name#* }" -metadata album="The Long Walk" -metadata artist="R. Rambler" \
		-metadata track="${name%% *}" -metadata disc="${disc#CD}"
done
printf 'Four short parts about a long walk.\n' > "The Long Walk/description.txt"
cover 0xe76f51 0xf4a261 "The Long Walk/folder.png"

# Untagged files with numbers that only sort correctly "naturally", and no
# artwork, so ReSpool draws a placeholder.
mkdir -p "Evening Lectures"
for i in 1 2 10; do
	tone 20 $((200 + i * 30)) "Evening Lectures/Lecture $i.mp3" -c:a libmp3lame -q:a 7
done

# A loose file in the root with embedded cover art.
cover 0x6d597a 0xb56576 art.jpg
ff -f lavfi -i "sine=frequency=520:duration=45" -i art.jpg -map 0:a -map 1:v -c:v copy \
	-ac 1 -ar 22050 -c:a libmp3lame -b:a 32k -id3v2_version 3 \
	-metadata title="A Short Story" -metadata album="A Short Story" -metadata artist="M. Brief" \
	-metadata:s:v title="Album cover" -metadata:s:v comment="Cover (front)" "A Short Story.mp3"
rm art.jpg

echo "Sample library written to $(pwd)"
