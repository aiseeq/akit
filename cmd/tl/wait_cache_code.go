package main

// waitCacheCode — sha256 исходников, от которых зависит запись кэша tl --wait: пакеты модуля из go list -deps
// ./cmd/tl (без тестов) и версии внешних модулей. Пишет и сверяет TestWaitCacheCodeMatchesSources.
const waitCacheCode = "6eea8c929b9a7e6cc25b8a15a7420c972eeb6cf3567dfd8cd8b93fdc16fb5598"
