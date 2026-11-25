package main

// waitCacheCode — sha256 исходников, от которых зависит запись кэша tl --wait: пакеты модуля из go list -deps
// ./cmd/tl (без тестов) и версии внешних модулей. Пишет и сверяет TestWaitCacheCodeMatchesSources.
const waitCacheCode = "45e6abd78f79b75c0703e8e4a3031f3d1014e21263f2de47b8407b0fe43d4927"
