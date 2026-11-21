package main

// waitCacheCode — sha256 исходников, от которых зависит запись кэша tl --wait: пакеты модуля из go list -deps
// ./cmd/tl (без тестов) и версии внешних модулей. Пишет и сверяет TestWaitCacheCodeMatchesSources.
const waitCacheCode = "49d0fc7d9a9c2d5c1cb8629398a5b8a5fad0a2ae10f0fa4f4c065cada2d2cde4"
