package main

// waitCacheCode — sha256 исходников, от которых зависит запись кэша tl --wait: пакеты модуля из go list -deps
// ./cmd/tl (без тестов) и версии внешних модулей. Пишет и сверяет TestWaitCacheCodeMatchesSources.
const waitCacheCode = "132bc8a9c27a0a65adf1ec65b3bc8e19bd0f3ac677a4df684f2f975f31f16ed9"
