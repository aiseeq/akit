package main

// waitCacheCode — sha256 исходников, от которых зависит запись кэша tl --wait: пакеты модуля из go list -deps
// ./cmd/tl (без тестов) и версии внешних модулей. Пишет и сверяет TestWaitCacheCodeMatchesSources.
const waitCacheCode = "7952a09c545065b57258f1f86e526afd8b1c57aea1a4a45f9a76e59e72b9f63d"
