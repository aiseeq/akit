package main

// waitCacheCode — sha256 исходников, от которых зависит запись кэша tl --wait: пакеты модуля из go list -deps
// ./cmd/tl (без тестов) и версии внешних модулей. Пишет и сверяет TestWaitCacheCodeMatchesSources.
const waitCacheCode = "00709381e8d66fc8adc95a1e2e5cf1a8eed5f991c6c7e0b0e4efae54dc3f6112"
