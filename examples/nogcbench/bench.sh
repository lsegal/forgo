#!/bin/sh
# Runs every workload as nogc code and as garbage collected code (the same
# binary, with -gc), each in its own process (so peak RSS is per workload), a few times, and
# prints the median run of each side by side as a Markdown table.
#
#	./bench.sh [scale [runs]]
set -eu
scale=${1:-4}
runs=${2:-5}
cd "$(dirname "$0")"
tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT
forgo build -o "$tmp/bench" .
workloads="binarytrees rbtree deriv wordfreq mandeltree chunks trie"

# median runs one workload $runs times, with flag $1 (-gc, or none for
# nogc), and prints the run with the median wall time.
median() {
	i=0
	while [ $i -lt "$runs" ]; do
		"$tmp/bench" -w "$2" -scale "$scale" $1
		i=$((i + 1))
	done | sort -k3 -n | sed -n "$(((runs + 1) / 2))p"
}

echo "scale $scale, median of $runs runs, $(uname -sm)"
echo
echo "| workload | gc wall | nogc wall | gc cpu | nogc cpu | gc GCs | gc max pause | gc peak RSS | nogc peak RSS | nogc reuses |"
echo "|---|---:|---:|---:|---:|---:|---:|---:|---:|---:|"
for w in $workloads; do
	g=$(median -gc "$w")
	n=$(median "" "$w")
	gsum=$(echo "$g" | awk '{print $NF}')
	nsum=$(echo "$n" | awk '{print $NF}')
	if [ "$gsum" != "$nsum" ]; then
		echo "$w: checksums differ ($gsum vs $nsum)" >&2
		exit 1
	fi
	echo "$g $n" | awk -v w="$w" '{
		# fields: mode name wall ms cpu ms cpu GCs GCs pause ms max pause rss MB peak RSS reused reused checksum sum
		n = NF / 2
		printf "| %s | %s ms | %s ms | %s ms | %s ms | %s | %s ms | %s MB | %s MB | %s |\n",
			w, $3, $(n+3), $5, $(n+5), $8, $10, $14, $(n+14), $(n+18)
	}'
done
