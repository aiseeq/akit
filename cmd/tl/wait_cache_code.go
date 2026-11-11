package main

// waitCacheCode — sha256 исходников, от которых зависит запись кэша tl --wait: пакеты модуля из go list -deps
// ./cmd/tl (без тестов) и версии внешних модулей. Пишет и сверяет TestWaitCacheCodeMatchesSources.
const waitCacheCode = "dd3fb0debf0c288d1bf7ba046cd97ebff88bf0f3e8f49c24c2c1a9101686ca32"
