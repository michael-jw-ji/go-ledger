import { corruptBalance } from './corruptBalance'
import { crashAfterCommit } from './crashAfterCommit'
import { doubleSpend } from './doubleSpend'
import { keyReuse } from './keyReuse'
import { redisWiped } from './redisWiped'
import { retryStorm } from './retryStorm'

export const scenarios = [retryStorm, doubleSpend, keyReuse, redisWiped, crashAfterCommit, corruptBalance]
