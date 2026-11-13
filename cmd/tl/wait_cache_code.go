package main

// waitCacheCode — sha256 исходников, от которых зависит запись кэша tl --wait: пакеты модуля из go list -deps
// ./cmd/tl (без тестов) и версии внешних модулей. Пишет и сверяет TestWaitCacheCodeMatchesSources.
const waitCacheCode = "c07feef2d0bd5ddd0ed8ed9f6a705ad3c912f79d63e1f11c4450c16a7347fc9c"
