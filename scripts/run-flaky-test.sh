#!/usr/bin/env bash
set -euo pipefail

# its value must satify: int && > 0
max_retry_times=5

if [ "$#" -ne 1 ]; then
  exit 1
fi

pkg="$1"
tmpfile=$(mktemp)

function clean_tmp_file(){
  #echo "clean_tmp_file $tmpfile $?"
  rm -f "$tmpfile"
}

trap clean_tmp_file EXIT

echo $pkg
go test -list=. "$pkg" | { grep -P "^Test" || test $? = 1 ; } > "$tmpfile"

# $1:testFp
# $2:pkg
function run_flaky_test(){
  local finalRet=1
  for retry_ct in $(seq 1 $max_retry_times)
  do
    echo "flaky test $1 in $2 retry_ct:$retry_ct"
    set +e
    go test -tags enable_flaky_tests -count=1 -run "$1" "$2"
    local ret="$?"
    set -e
    if [ "$ret" -eq 0 ]
    then
      finalRet=0
      break
    fi
  done
  if [ "$finalRet" -ne "0" ]
  then
    exit $finalRet
  fi
  #
  return $finalRet 
}

go test -tags enable_flaky_tests -list=. $pkg | { grep -P "^Test" || test $? = 1 ; } | { grep -v -Fwf $tmpfile || test $? = 1; } | while read testFp
do
  run_flaky_test "$testFp" "$pkg"
done
