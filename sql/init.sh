#!/bin/bash

CURRENT_DIR=$(cd $(dirname $0);pwd)
cd $CURRENT_DIR

cat ./init_comment_count.sql | mysql -u isuconp -pisuconp isuconp
