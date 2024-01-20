#!/bin/bash

make "$1-$2"

TARGETFILE='mieru'
if [ "$1" = "server-linux" ]; then
    TARGETFILE='mita'
fi
mkdir /release

cp "/src/release/linux/$2/$TARGETFILE" /release
echo "#!/bin/sh

./$TARGETFILE apply config /config/$TARGETFILE.json && ./$TARGETFILE run
exit 0" > /release/entrypoint.sh

chmod +x /release/entrypoint.sh