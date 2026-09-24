#!/usr/bin/env bats

# What the module says, not whether it parses: `make lint` runs fmt, init
# without a backend and validate over both the module and the example, and is
# the single lint path for them. These tests read the files, so they need no
# terraform on PATH.

setup() {
  cd "$BATS_TEST_DIRNAME/../terraform/gcp" || return 1
}

@test "the two shipped files are embedded from this directory, not fetched by default" {
  # -F: the literals contain `$`, which macOS grep reads as an anchor.
  # shellcheck disable=SC2016  # the ${path.module} is Terraform's, matched literally with -F
  grep -qF 'file("${path.module}/../../compose/compose.yaml")' main.tf
  # shellcheck disable=SC2016
  grep -qF 'file("${path.module}/../../compose/.env")' main.tf
  # The template assembles the URL from $MD, so match its parts.
  grep -q 'computeMetadata/v1/instance/attributes' startup.sh.tftpl
  grep -q 'docker compose up -d --wait' startup.sh.tftpl
  # Each metadata key is written in main.tf and read in the template, so a
  # rename in one file alone leaves the boot fetching a key nothing wrote.
  for key in e2b-compose-yaml e2b-dot-env; do
    grep -qF "$key" main.tf || { echo "$key is not in main.tf"; return 1; }
    grep -qF "$key" startup.sh.tftpl || { echo "$key is not in startup.sh.tftpl"; return 1; }
  done
}

@test "the startup script never prints the secrets it writes" {
  # The three secrets reach the instance in its .env, and the startup script's
  # output is the serial console and Cloud Logging, both readable with far less
  # than the project access the state file needs. `compose config` renders the
  # interpolated file, `set -x` traces every assignment and a plain cat of the
  # .env prints all three, so none of them may appear either.
  run grep -nE 'docker compose logs|docker compose config|set -x|cat[[:space:]]+\.env' startup.sh.tftpl
  [ "$status" -eq 1 ]
}

# The startup script deletes seven keys from the shipped .env and appends its
# own, which is what puts a Terraform install on the secrets in its state and
# the sizing and telemetry its variables ask for. compose.yaml reads each of
# them but one as ${KEY:-...}, so renaming one there and not here would
# silently drop the appended line: the file still parses, the stack still
# starts, and the install runs on what compose does when nothing is set -- its
# own generated team key and api secrets, which the state does not know, or
# the hugepage default rather than the requested one. The one is
# COMPOSE_PROFILES, which Compose itself reads from the project's .env and
# compose.yaml never interpolates; what has to exist for it is the profile the
# module writes into it.
@test "the startup script writes only .env keys the compose project reads" {
  keys="$(awk '/^  cat >> \.env <<EOF$/ { f = 1; next } f && /^EOF$/ { exit } f' \
    startup.sh.tftpl | sed -n 's/^\([A-Z_][A-Z0-9_]*\)=.*/\1/p')"
  # An anchor that stopped matching would pass the test vacuously.
  [ -n "$keys" ]

  # The sed just above the heredoc strips the shipped value of each key before
  # the override is appended. The two lists have to be the same: a key appended
  # but not stripped leaves the shipped default sitting above the override in
  # the same file, and a key stripped but not appended drops it altogether.
  deleted="$(grep -F 'sed -i -E' startup.sh.tftpl |
    grep -oE '\([A-Z0-9_|]+\)' | tr -d '()' | tr '|' '\n' | sort)"
  [ -n "$deleted" ]
  diff <(printf '%s\n' "$deleted") <(printf '%s\n' "$keys" | sort) || {
    echo "the sed delete list (-) and the appended keys (+) differ"
    return 1
  }

  while read -r key; do
    if [ "$key" = COMPOSE_PROFILES ]; then
      grep -qF 'var.otel_collector ? "otel" : ""' main.tf || {
        echo "main.tf no longer writes COMPOSE_PROFILES as otel or nothing"
        return 1
      }
      grep -qxF '    profiles: [otel]' ../../compose/compose.yaml || {
        echo "the startup script can turn on the otel profile, which no compose.yaml service has"
        return 1
      }
      continue
    fi
    # -F: the literal contains `$`, which macOS grep reads as an anchor.
    grep -qF "\${$key" ../../compose/compose.yaml || {
      echo "the startup script appends $key, which compose.yaml never reads"
      return 1
    }
  done <<<"$keys"
}

# Two variables, one setting: an endpoint of the operator's own wins, and the
# built-in collector implies its own loopback address when none is given.
@test "the collector variables reach the instance's .env" {
  # shellcheck disable=SC2016  # the ${...} are the template's, matched literally with -F
  grep -qxF 'E2B_OTEL_COLLECTOR_GRPC_ENDPOINT=${otel_endpoint}' startup.sh.tftpl
  # shellcheck disable=SC2016
  grep -qxF 'COMPOSE_PROFILES=${compose_profiles}' startup.sh.tftpl
  # Squeezed, so the alignment `terraform fmt` picks does not matter.
  tr -s ' ' < main.tf | grep -qF 'otel_endpoint = var.otel_collector_grpc_endpoint != "" ? var.otel_collector_grpc_endpoint : (var.otel_collector ? "127.0.0.1:4317" : "")'
  tr -s ' ' < main.tf | grep -qF 'compose_profiles = var.otel_collector ? "otel" : ""'
  for var in otel_collector_grpc_endpoint otel_collector; do
    grep -qx "variable \"$var\" {" variables.tf || { echo "variables.tf has no $var"; return 1; }
    grep -qF "| \`$var\` |" README.md || { echo "the README's variables table has no $var"; return 1; }
  done
  # The address the built-in collector implies is the one its receiver binds.
  grep -qx '        endpoint: 127.0.0.1:4317' ../../compose/config/otel/otel-collector.yaml
}

# The services take host:port and nothing else, and the value lands unquoted
# in the instance's .env at first boot, so the variable refuses anything else
# at plan time rather than after a replace. Checked with the rule's own
# regex, which ERE reads the way Terraform's RE2 does.
@test "the endpoint variable takes host:port and refuses a URL" {
  re="$(sed -n 's/.*can(regex("\(.*\)", var\.otel_collector_grpc_endpoint)).*/\1/p' variables.tf |
    sed 's/\\\\/\\/g')"
  # A pattern that stopped matching would pass the refusals vacuously.
  [ -n "$re" ]
  grep -qF 'condition     = var.otel_collector_grpc_endpoint == "" ||' variables.tf
  for ok in collector:4317 10.0.0.5:4317 '[::1]:4317' 127.0.0.1:4317; do
    grep -qE "$re" <<<"$ok" || { echo "refuses $ok"; return 1; }
  done
  # shellcheck disable=SC2016  # a literal $(...), which the heredoc would run
  for bad in http://collector:4317 collector collector:4317/ 'a b:1' '$(id):1'; do
    run grep -qE "$re" <<<"$bad"
    [ "$status" -eq 1 ] || { echo "accepts $bad"; return 1; }
  done
}

# The comment at the top of firewall.tf is the whole reasoning for the one rule
# an operator's CIDRs reach: it lists every port the stack binds and then names
# the few that are opened. Nothing else ties that prose to the HCL under it, so
# a port added to the rule and not the sentence -- or dropped from the sentence
# and left in the rule -- leaves the file arguing with itself, and the next
# reader deciding what the module exposes believes the sentence.
@test "the firewall header names the ports the clients rule opens" {
  header="$(sed -e '/^[^#]/,$d' -e 's/^#[[:space:]]*//' firewall.tf | tr '\n' ' ')"
  documented="$(printf '%s\n' "$header" |
    sed -n 's/.*[[:space:]]only \(.*\) are opened.*/\1/p' |
    grep -oE '[0-9]+' | sort -n)"
  # An anchor that stopped matching would pass the test vacuously.
  [ -n "$documented" ]

  # The clients rule's own allow block, the first one in the file.
  opened="$(awk '/"google_compute_firewall" "clients"/ { f = 1 }
                 f && /ports +=/ { print; exit }' firewall.tf |
    grep -oE '[0-9]+' | sort -n)"
  [ -n "$opened" ]

  diff <(printf '%s\n' "$documented") <(printf '%s\n' "$opened") || {
    echo "the header comment (-) and the clients rule (+) name different ports"
    return 1
  }
}
