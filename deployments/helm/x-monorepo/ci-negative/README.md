Profiles that must NOT render. `task helm-check` runs `helm template` on each and requires a
non-zero exit whose output contains the text after `# expect:` on the file's first line
(deployments/helm/check-negative.sh).
