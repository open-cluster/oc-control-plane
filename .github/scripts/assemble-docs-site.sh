#!/bin/sh
# Assembles the published documentation tree in place: this repository owns docs.json and
# every MDX page; the site repository keeps its chrome (logo, favicon, licence). One
# script, used by both the validate and the sync job, so what is validated is exactly
# what is published.
set -eu

site="$1"

find "$site" -name '*.mdx' -delete
rm -f "$site/docs.json"
cp -r docs/. "$site/"
