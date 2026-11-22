package main

// waitCacheCode — sha256 исходников, от которых зависит запись кэша tl --wait: пакеты модуля из go list -deps
// ./cmd/tl (без тестов) и версии внешних модулей. Пишет и сверяет TestWaitCacheCodeMatchesSources.
const waitCacheCode = "ba7f7bccb5c15e2482870df47cecf0076e9b81fc0d859dc1b8cb9ed59bee4dfd"
