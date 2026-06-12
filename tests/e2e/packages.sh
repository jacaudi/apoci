#!/bin/sh
# Publish a package to each backend with its real client (npm, cargo, twine,
# dotnet), then read it back.
set -u

ALICE="${ALICE:-http://alice.test:5000}"
TOKEN="${ALICE_REGISTRY_TOKEN:-alice-registry-token}"
# host:port without scheme, for the npm _authToken key
ALICE_HOST=$(echo "$ALICE" | sed -e 's#^https\?://##')

PASS=0
FAIL=0
pass() { PASS=$((PASS + 1)); echo "  PASS: $1"; }
fail() { FAIL=$((FAIL + 1)); echo "  FAIL: $1"; }

# verify_url NAME EXPECTED URL [curl args...] — GET URL, assert body contains EXPECTED.
verify_url() {
  name="$1"; shift
  expected="$1"; shift
  body=$(curl -sf "$@") || { fail "$name (curl error)"; return; }
  case "$body" in
    *"$expected"*) pass "$name" ;;
    *) fail "$name — expected '$expected', got: $body" ;;
  esac
}

WORK=/tmp/pkg-e2e
rm -rf "$WORK"
mkdir -p "$WORK"

echo ""
echo "=== Package Publish E2E (real clients vs alice) ==="

# ==============================================================
echo ""
echo "--- npm: npm publish ---"
NPM_DIR="$WORK/npm"
mkdir -p "$NPM_DIR"
cat > "$NPM_DIR/package.json" <<EOF
{ "name": "apoci-demo-npm", "version": "1.0.0", "description": "e2e", "license": "MIT" }
EOF
echo "module.exports = 42;" > "$NPM_DIR/index.js"
cat > "$NPM_DIR/.npmrc" <<EOF
registry=$ALICE/npm/
//$ALICE_HOST/npm/:_authToken=$TOKEN
EOF
if (cd "$NPM_DIR" && npm publish --userconfig ./.npmrc >/dev/null 2>&1); then
  pass "npm publish"
else
  fail "npm publish"
fi
verify_url "npm packument readable" '"apoci-demo-npm"' "$ALICE/npm/apoci-demo-npm"
verify_url "npm tarball downloadable" '' "$ALICE/npm/apoci-demo-npm/-/apoci-demo-npm-1.0.0.tgz"

# ==============================================================
echo ""
echo "--- cargo: cargo publish ---"
CARGO_DIR="$WORK/apoci_demo_cargo"
cargo new --lib "$CARGO_DIR" -q 2>/dev/null
cat > "$CARGO_DIR/Cargo.toml" <<EOF
[package]
name = "apoci_demo_cargo"
version = "0.1.0"
edition = "2021"
description = "e2e"
license = "MIT"

[dependencies]
EOF
mkdir -p "$CARGO_DIR/.cargo"
cat > "$CARGO_DIR/.cargo/config.toml" <<EOF
[registries.apoci]
index = "sparse+$ALICE/cargo/"

[registry]
global-credential-providers = ["cargo:token"]
EOF
if (cd "$CARGO_DIR" && CARGO_REGISTRIES_APOCI_TOKEN="$TOKEN" \
    cargo publish --registry apoci --no-verify --allow-dirty >/dev/null 2>&1); then
  pass "cargo publish"
else
  fail "cargo publish"
fi
verify_url "cargo sparse index entry" '"apoci_demo_cargo"' \
  -H "Authorization: $TOKEN" "$ALICE/cargo/ap/oc/apoci_demo_cargo"
verify_url "cargo crate downloadable" '' \
  -H "Authorization: $TOKEN" "$ALICE/cargo/api/v1/crates/apoci_demo_cargo/0.1.0/download"

# ==============================================================
echo ""
echo "--- pypi: twine upload ---"
PY_DIR="$WORK/pypi"
mkdir -p "$PY_DIR/src/apoci_demo_pypi"
cat > "$PY_DIR/pyproject.toml" <<EOF
[build-system]
requires = ["setuptools>=61"]
build-backend = "setuptools.build_meta"

[project]
name = "apoci-demo-pypi"
version = "1.0.0"
description = "e2e"
license = "MIT"
EOF
echo "x = 42" > "$PY_DIR/src/apoci_demo_pypi/__init__.py"
if (cd "$PY_DIR" && python3 -m build --no-isolation >/dev/null 2>&1 && \
    twine upload --repository-url "$ALICE/pypi/" -u __token__ -p "$TOKEN" dist/* >/dev/null 2>&1); then
  pass "twine upload"
else
  fail "twine upload"
fi
verify_url "pypi simple index lists project" 'apoci_demo_pypi-1.0.0' \
  "$ALICE/pypi/simple/apoci-demo-pypi/"

# ==============================================================
echo ""
echo "--- nuget: dotnet nuget push ---"
NUGET_DIR="$WORK/nuget"
mkdir -p "$NUGET_DIR"
export DOTNET_CLI_TELEMETRY_OPTOUT=1 DOTNET_NOLOGO=1 DOTNET_CLI_HOME="$NUGET_DIR"
cat > "$NUGET_DIR/ApociDemoNuget.csproj" <<EOF
<Project Sdk="Microsoft.NET.Sdk">
  <PropertyGroup>
    <TargetFramework>net8.0</TargetFramework>
    <PackageId>ApociDemoNuget</PackageId>
    <Version>1.0.0</Version>
    <Authors>e2e</Authors>
    <Description>e2e</Description>
  </PropertyGroup>
</Project>
EOF
if (cd "$NUGET_DIR" && dotnet pack -c Release -o ./out >/dev/null 2>&1 && \
    dotnet nuget push ./out/ApociDemoNuget.1.0.0.nupkg \
      -s "$ALICE/nuget/v3/index.json" -k "$TOKEN" >/dev/null 2>&1); then
  pass "dotnet nuget push"
else
  fail "dotnet nuget push"
fi
verify_url "nuget flat-container version list" '"1.0.0"' \
  "$ALICE/nuget/v3-flatcontainer/apocidemonuget/index.json"
verify_url "nuget package downloadable" '' \
  "$ALICE/nuget/v3-flatcontainer/apocidemonuget/1.0.0/apocidemonuget.1.0.0.nupkg"

# ==============================================================
echo ""
echo "=== Package Results: $PASS passed, $FAIL failed ==="
echo ""

if [ "$FAIL" -gt 0 ]; then
  exit 1
fi
