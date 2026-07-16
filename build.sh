#!/usr/bin/env bash
set -euo pipefail

APP="GrokDesktop"
BUILD_DIR="build/bin"

usage() {
  echo "Build interativo — GrokDesktop"
  echo ""
  echo "Plataformas:"
  echo "  1) Linux   (amd64)"
  echo "  2) Windows (amd64, WebView2 embutido)"
  echo "  3) macOS   (amd64)"
  echo "  4) Todas   (Linux + Windows + macOS)"
  echo ""
}

detect_tags() {
  case "$(uname -s)" in
    Linux*)  echo "-tags webkit2_41" ;;
    *)       echo "" ;;
  esac
}

build_linux() {
  echo "=== Build Linux amd64 ==="
  wails build -platform linux/amd64 -clean -o "$APP-linux" $(detect_tags)
  echo "→ $BUILD_DIR/$APP-linux"
}

build_windows() {
  echo "=== Build Windows amd64 ==="
  wails build -platform windows/amd64 -clean -o "$APP-windows.exe" -webview2 embed
  echo "→ $BUILD_DIR/$APP-windows.exe"
}

build_darwin() {
  echo "=== Build macOS amd64 ==="
  wails build -platform darwin/amd64 -clean -o "$APP-macos"
  echo "→ $BUILD_DIR/$APP-macos"
}

check_deps() {
  local missing=false

  if ! command -v go &>/dev/null; then
    echo "✗ Go não encontrado. Instale: https://go.dev/dl/"
    missing=true
  fi

  if ! command -v wails &>/dev/null; then
    echo "✗ Wails CLI não encontrado. Instale: go install github.com/wailsapp/wails/v2/cmd/wails@latest"
    missing=true
  fi

  if ! command -v node &>/dev/null; then
    echo "✗ Node.js não encontrado. Instale: https://nodejs.org/"
    missing=true
  fi

  if ! command -v npm &>/dev/null; then
    echo "✗ npm não encontrado. Instale com Node.js: https://nodejs.org/"
    missing=true
  fi

  case "$(uname -s)" in
    Linux*)
      if ! pkg-config --exists webkit2gtk-4.1 2>/dev/null; then
        echo "✗ webkit2gtk-4.1 não encontrado."
        echo "  Debian/Ubuntu: sudo apt-get install libwebkit2gtk-4.1-dev libgtk-3-dev"
        missing=true
      fi
      if ! pkg-config --exists gtk+-3.0 2>/dev/null; then
        echo "✗ GTK3 não encontrado."
        echo "  Debian/Ubuntu: sudo apt-get install libgtk-3-dev"
        missing=true
      fi
      ;;
  esac

  if [ "$missing" = true ]; then
    echo ""
    echo "Corrija as dependências acima e tente novamente."
    exit 1
  fi
}

interactive() {
  echo "╔══════════════════════════╗"
  echo "║  GrokDesktop — Build    ║"
  echo "╚══════════════════════════╝"
  echo ""
  check_deps
  echo ""
  echo "Escolha a plataforma:"
  echo "  1) Linux"
  echo "  2) Windows"
  echo "  3) macOS"
  echo "  4) Todas"
  echo ""
  read -rp "Opção [1-4]: " opt

  mkdir -p "$BUILD_DIR"

  case "$opt" in
    1) build_linux ;;
    2) build_windows ;;
    3) build_darwin ;;
    4)
      build_linux
      echo ""
      build_windows
      echo ""
      build_darwin
      ;;
    *)
      echo "Opção inválida: $opt"
      exit 1
      ;;
  esac

  echo ""
  echo "Build concluído! Arquivos em $BUILD_DIR:"
  ls -lh "$BUILD_DIR"
}

interactive