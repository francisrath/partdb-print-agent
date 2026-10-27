# Per-package statement coverage from a `go test -coverpkg=./...` profile, as a Markdown table.
# A block can appear once per test binary; it counts once and is covered if any run hit it.
NR > 1 {
  key = $1; file = $1; sub(/:.*/, "", file)
  n[key] = $2; f[key] = file
  if ($3 > 0) hit[key] = 1
}
END {
  for (k in n) {
    p = f[k]; sub(/\/[^\/]*$/, "", p); sub(/^[^\/]*\/[^\/]*\/[^\/]*\/?/, "", p)
    if (p == "") p = "."
    t[p] += n[k]; T += n[k]
    if (k in hit) { c[p] += n[k]; C += n[k] }
  }
  print "| Package | Statements | Coverage |"
  print "|---|---:|---:|"
  for (p in t) printf "| %s | %d | %.1f%% |\n", p, t[p], 100 * c[p] / t[p] | "sort"
  close("sort")
  printf "| **total** | **%d** | **%.1f%%** |\n", T, 100 * C / T
}
