<#
.SYNOPSIS
    创建或更新 iSCSI 目标（授权列表 / 启用状态 / 描述 / 单向与反向 CHAP）。

.DESCRIPTION
    关键语义（务必注意）：
      - 模块没有 Add-InitiatorId / Remove-InitiatorId 这类增量 cmdlet，
        `-InitiatorIds` 是**全量替换**（overwrite），不是增量追加。
        调用方（上层业务）负责"先读后合并"，本脚本只做整体回写。
      - 目标已存在时走 Set-IscsiServerTarget，不存在时走 New-IscsiServerTarget（upsert）。
      - 目标名可能含 `-`（不是合法的 PowerShell 标识符），因此所有值都通过
        `-File <脚本> -Name value` 传入，不做任何字符串拼接。

.NOTES
    CHAP 密钥只作为参数使用，绝不写入输出或日志。
    `-Chap` / `-ReverseChap` 的参数类型（PSCredential 或 "user,secret" 字符串）在不同版本上
    存在差异 —— 需实测确认；脚本已按运行时探测到的参数类型自适应。
#>

param(
    [string]$TargetName,
    [string]$InitiatorIdsJson = '',   # JSON 数组（如 ["IQN:...","IPAddress:1.2.3.4"]）；空串=不改动，[]=全量清空
    [string]$Enabled = '',            # 'true' / 'false'；空串=不改动
    [string]$Description = '',        # 空串=不改动
    [string]$ChapUser = '',
    [string]$ChapSecret = '',
    [string]$EnableReverseChap = '',  # 'true' 时启用反向 CHAP
    [string]$ReverseChapUser = '',
    [string]$ReverseChapSecret = ''
)

$ErrorActionPreference = 'Stop'
$ProgressPreference = 'SilentlyContinue'
try { [Console]::OutputEncoding = [System.Text.Encoding]::UTF8 } catch { }

# 判断 cmdlet 是否声明了某个参数。
#
# ⚠️ 绝不能用 $Parameters.Contains('X')：$CommandInfo.Parameters 的实际类型是
# Dictionary<string,ParameterMetadata>，它把非泛型 IDictionary.Contains **显式实现**，
# PowerShell 看不到，运行期报
#   Cannot find an overload for "Contains" and the argument count: "1"。
# 该错误曾在 set_chap 这一步稳定复现（MethodException）。优先 ContainsKey，再兜底 Contains。
function Test-ParameterExists {
    param(
        [System.Collections.IDictionary]$Parameters,
        [string]$Name
    )
    if (-not $Parameters) { return $false }
    try { if ($Parameters.ContainsKey($Name)) { return $true } } catch { }
    try { if ($Parameters.Contains($Name)) { return $true } } catch { }
    return $false
}

# 取参数的元素类型（数组取元素类型，非数组取自身）；参数不存在时返回 $null。
function Get-ParameterElementType {
    param(
        [System.Collections.IDictionary]$Parameters,
        [string]$Name
    )
    if (-not (Test-ParameterExists -Parameters $Parameters -Name $Name)) { return $null }
    try {
        $type = $Parameters[$Name].ParameterType
    } catch {
        return $null
    }
    if ($type.IsArray) { return $type.GetElementType() }
    return $type
}

# 把 "类型:值" 形式的授权标识转换成目标参数**实际需要**的类型。
#
# ⚠️ 实测（Windows Server，InitiatorId 的真实签名）：
#   ctors: Void .ctor(System.String)          ← **只有这一个**
#   props: Method(MethodType), Value(String)
# 也就是说必须把**完整的 "IPAddress:127.0.0.1" 字符串**交给单参构造，由它自己解析出
# Method/Value；既不能传裸字符串给参数（会被拒），也没有无参构造（New-Object 会报
# "A constructor was not found"）。
# 因此按元素类型分流：
#   - string / string[]（部分版本存在字符串版参数）→ 原样传字符串；
#   - 其它命令类型 → 反射构造，优先级：
#       ① 单参 (string)：整串喂进去（本例走这条）
#       ② 双参 (IdType, Value)
#       ③ 无参构造 + 属性赋值（老版本兼容）
function ConvertTo-InitiatorIdValues {
    param(
        [string[]]$Raw,
        [type]$ElementType
    )
    if ($null -eq $ElementType -or $ElementType -eq [string]) { return ,@($Raw) }

    $list = New-Object System.Collections.ArrayList
    foreach ($item in $Raw) {
        $text = [string]$item
        $idx = $text.IndexOf(':')
        if ($idx -le 0) {
            throw ("InitiatorIds 需要 '类型:值' 形式（如 IQN:iqn.1991-05.com.microsoft:client），实际收到: " + $text)
        }
        $idType = $text.Substring(0, $idx)
        $value = $text.Substring($idx + 1)

        $obj = $null
        $constructors = @($ElementType.GetConstructors())

        # ① 单参字符串构造：直接把整串交给它解析（实测 InitiatorId 只有这一个构造）。
        foreach ($ctor in $constructors) {
            $ctorParams = @($ctor.GetParameters())
            if ($ctorParams.Count -ne 1 -or $ctorParams[0].ParameterType -ne [string]) { continue }
            try {
                $obj = $ctor.Invoke(@($text))
                break
            } catch { $obj = $null }
        }

        # ② 双参构造 (IdType, Value)。
        if (-not $obj) {
            foreach ($ctor in $constructors) {
                $ctorParams = @($ctor.GetParameters())
                if ($ctorParams.Count -ne 2) { continue }
                try {
                    $argv = @($idType, $value)
                    if ($ctorParams[0].ParameterType.IsEnum) {
                        $argv[0] = [System.Enum]::Parse($ctorParams[0].ParameterType, $idType, $true)
                    }
                    if ($ctorParams[1].ParameterType -eq [string]) { $argv[1] = $value }
                    $obj = $ctor.Invoke($argv)
                    break
                } catch { $obj = $null }
            }
        }

        if (-not $obj) {
            $obj = New-Object -TypeName $ElementType.FullName
            $assigned = $false
            foreach ($propertyName in @('IdType', 'Type')) {
                $prop = $ElementType.GetProperty($propertyName)
                if ($prop -and $prop.CanWrite) {
                    if ($prop.PropertyType.IsEnum) {
                        $prop.SetValue($obj, [System.Enum]::Parse($prop.PropertyType, $idType, $true))
                    } else {
                        $prop.SetValue($obj, $idType)
                    }
                    $assigned = $true
                    break
                }
            }
            foreach ($propertyName in @('Value', 'Id')) {
                $prop = $ElementType.GetProperty($propertyName)
                if ($prop -and $prop.CanWrite) { $prop.SetValue($obj, $value); $assigned = $true; break }
            }
            # 构造不出来就明确报错：绝不把空对象塞给 cmdlet（那会变成更难查的隐性失败）。
            if (-not $assigned) {
                $signatures = @($constructors | ForEach-Object { $_.ToString() }) -join ' / '
                throw ("无法把 '" + $text + "' 转换成 " + $ElementType.FullName +
                    "：既没有单参 (string) / 双参 (IdType, Value) 构造，也没有可写的 IdType/Value 属性。可用构造: " + $signatures)
            }
        }

        [void]$list.Add($obj)
    }
    return ,@($list.ToArray())
}

# 把 CHAP 账号转换为 -Chap 参数所需的类型（PSCredential 或 "user,secret" 字符串）。
function New-ChapCredentialSplat {
    param(
        [System.Collections.IDictionary]$Parameters,
        [string]$User,
        [string]$Secret
    )
    $type = $null
    if (Test-ParameterExists -Parameters $Parameters -Name 'Chap') { $type = $Parameters['Chap'].ParameterType }
    if ($type -eq [string]) {
        return @{ Chap = ('{0},{1}' -f $User, $Secret) }
    }
    $secure = ConvertTo-SecureString -String $Secret -AsPlainText -Force
    $credential = New-Object System.Management.Automation.PSCredential($User, $secure)
    return @{ Chap = $credential }
}

# 把反向 CHAP 账号转换为 -ReverseChap 参数所需的类型。
function New-ReverseChapCredentialSplat {
    param(
        [System.Collections.IDictionary]$Parameters,
        [string]$User,
        [string]$Secret
    )
    if (-not (Test-ParameterExists -Parameters $Parameters -Name 'ReverseChap')) {
        throw '当前系统的 Set-IscsiServerTarget 不支持 -ReverseChap 参数（需实测确认）'
    }
    $type = $Parameters['ReverseChap'].ParameterType
    if ($type -eq [string]) {
        return @{ ReverseChap = ('{0},{1}' -f $User, $Secret) }
    }
    $secure = ConvertTo-SecureString -String $Secret -AsPlainText -Force
    $credential = New-Object System.Management.Automation.PSCredential($User, $secure)
    return @{ ReverseChap = $credential }
}

try {
    # $step 记录"当前进行到哪一步"：失败时原样上报。
    # 之所以必须带上它：本脚本的所有失败都归为 set_target_failed，
    # 只看 reason 无法分辨是"建目标失败"还是"启用失败"还是"CHAP 失败"，
    # 而它们的处置方式完全不同（见 docs/windows-iscsi-vhdx-api.md 5.2）。
    $step = 'validate'
    if (-not $TargetName) { throw 'TargetName 参数不能为空' }
    if ($ChapUser -or $ChapSecret) {
        if (-not ($ChapUser -and $ChapSecret)) { throw 'ChapUser 与 ChapSecret 必须同时提供' }
    }

    $step = 'load_module'
    Import-Module -Name IscsiTarget -ErrorAction Stop
    $setCommand = Get-Command -Name Set-IscsiServerTarget -ErrorAction Stop

    # 查找现有目标：**枚举后比对**，而不是依赖 `-TargetName` 精确查询。
    # 原因：`Get-IscsiServerTarget -TargetName X` 查不到时会抛错，配合 SilentlyContinue 会静默变成
    # "$target = $null"，于是脚本以为"不存在"去建，最后以"目标已存在"的形式失败——
    # 这种现象表现为"每一次都失败且看不出原因"。枚举比对规避了这条静默链路，
    # 同时兼容"Windows 把短名保存成 iqn.<...>:<短名>"的形态。
    $step = 'get_target'
    $target = $null
    foreach ($candidate in @(Get-IscsiServerTarget -ErrorAction SilentlyContinue)) {
        # PowerShell 的 -eq 对字符串本身即大小写不敏感。
        if ($candidate.TargetName -eq $TargetName -or
            $candidate.TargetName -eq "iqn.1991-05.com.microsoft:$TargetName") {
            $target = $candidate
            break
        }
    }

    if (-not $target) {
        $createArgs = @{ TargetName = $TargetName }
        if ($Description) { $createArgs['Description'] = $Description }
        if ($InitiatorIdsJson -ne '') {
            # ⚠️ 同 volume_find_iscsi_disk.ps1 的坑：PS 5.1 的 ConvertFrom-Json 把 JSON 数组
            # 整体当一个对象输出（不展开），直接进管道会让 `.Count` 恒为 1、元素还是数组，
            # 于是"多个 initiator"被压成一个（拼接后的脏字符串）下发。先对变量展开再过滤。
            $decodedIds = ConvertFrom-Json -InputObject $InitiatorIdsJson
            $initialIds = @($decodedIds | Where-Object { $_ })
            if ($initialIds.Count -gt 0) {
                # 建目标同样要按参数类型转换（否则同样报 Cannot convert ... to InitiatorId）。
                $newCommand = Get-Command -Name New-IscsiServerTarget -ErrorAction Stop
                $createArgs['InitiatorIds'] = ConvertTo-InitiatorIdValues -Raw $initialIds `
                    -ElementType (Get-ParameterElementType -Parameters $newCommand.Parameters -Name 'InitiatorIds')
            }
        }
        $step = 'create_target'
        try {
            New-IscsiServerTarget @createArgs -ErrorAction Stop | Out-Null
            $action = 'created'
        } catch {
            # 建失败后再查一次：并发/前次残留导致"其实已经存在"时，按成功处理（幂等优先）。
            $exists = @(Get-IscsiServerTarget -ErrorAction SilentlyContinue) |
                Where-Object { $_.TargetName -eq $TargetName -or
                               $_.TargetName -eq "iqn.1991-05.com.microsoft:$TargetName" }
            if (-not $exists) { throw }
            $target = $exists | Select-Object -First 1
            $action = 'exists'
        }
    } else {
        $action = 'updated'
        if ($Description) {
            $step = 'set_description'
            Set-IscsiServerTarget -TargetName $TargetName -Description $Description -ErrorAction Stop
        }
    }

    # 授权列表：**始终**下发（整体覆盖）。
    #
    # ⚠️ 空列表/不带 -InitiatorIds 在 Windows 目标服务器上都等于**拒绝所有 initiator**，
    # 客户端登录报 "The target name is not found or is marked as hidden from login"
    # （真实事故：目标已启用、已映射、名字也读回正确，唯独 initiator 列表为空 → 连不上）。
    # 空的真实语义应是"不限制（open）"，在 Windows 上要用通配 `IQN:*` 表达"任意 initiator"。
    # 访问控制由单向 CHAP 负责。
    $bindMode = 'unchanged'
    if ($InitiatorIdsJson -ne '') {
        # 同上：必须先在变量上展开成元素，否则多客户端共享同一目标时白名单只下发第一个。
        $decodedIds = ConvertFrom-Json -InputObject $InitiatorIdsJson
        $ids = @($decodedIds | Where-Object { $_ })
        if ($ids.Count -eq 0) {
            # 空 = open：下发通配"任意 initiator"，而不是留一个拒绝所有的空列表。
            $ids = @('IQN:*')
        }

        $step = 'set_initiators'

        # 参数名与元素类型按运行时探测：
        #   - 若存在"接受字符串"的参数（部分版本把 -InitiatorId 做成 string[]），优先用它；
        #   - 否则用 -InitiatorIds，并按元素类型把字符串转成命令对象（见 ConvertTo-InitiatorIdValues）。
        $paramName = 'InitiatorIds'
        $elementType = Get-ParameterElementType -Parameters $setCommand.Parameters -Name 'InitiatorIds'
        foreach ($candidate in @('InitiatorId', 'InitiatorIds')) {
            $candidateType = Get-ParameterElementType -Parameters $setCommand.Parameters -Name $candidate
            if ($candidateType -eq [string]) {
                $paramName = $candidate
                $elementType = $candidateType
                break
            }
        }

        $values = ConvertTo-InitiatorIdValues -Raw $ids -ElementType $elementType
        if ($elementType -eq [string]) {
            $bindMode = 'string'
        } elseif ($elementType) {
            $bindMode = 'object:' + $elementType.Name
        } else {
            $bindMode = 'raw'
        }

        $setArgs = @{ TargetName = $TargetName }
        $setArgs[$paramName] = $values
        Set-IscsiServerTarget @setArgs -ErrorAction Stop
    }

    # 启用 / 停用
    #
    # ⚠️ 顺序约束（Windows 侧实测语义）：目标必须**先有已映射的虚拟盘**才能被启用，
    # 因此调用方（winbackend.EnsureTarget）把 AddMapping 放在启用之前。
    #
    # ⚠️ 必须用 -Enable:$true 冒号语法：同一个 cmdlet 上 -EnableChap:$true（冒号）实测生效，
    # 而 -Enable $true（空格）**不报错但不生效**，目标停留在 Disabled，客户端登录报
    # "target name is not found or is marked as hidden from login"（真实事故）。
    # 冒号语法对 switch 与 bool 两种参数形态都正确绑定。
    if ($Enabled -ne '') {
        $step = 'set_enabled'
        $enableFlag = [System.Boolean]::Parse($Enabled)
        if ($enableFlag) {
            Set-IscsiServerTarget -TargetName $TargetName -Enable:$true -ErrorAction Stop
        } else {
            Set-IscsiServerTarget -TargetName $TargetName -Enable:$false -ErrorAction Stop
        }

        # 回读验证：启用/停用必须真的生效。静默失败曾让目标停留在 Disabled，
        # 而上层日志只看到 action=updated，无从排查（真实事故）。
        $verify = Get-IscsiServerTarget -TargetName $TargetName -ErrorAction Stop
        $verifyStatus = ''
        try { $verifyStatus = [string]$verify.Status } catch { }
        $looksDisabled = $verifyStatus -match '^(Disabled|Stopped|Offline|Inactive)$'
        if ($enableFlag -and $looksDisabled) {
            throw ("目标启用未生效（Status=$verifyStatus）。已尝试 -Enable:`$true，请检查 Set-IscsiServerTarget 的参数形态")
        }
        if (-not $enableFlag -and -not $looksDisabled) {
            throw ("目标停用未生效（Status=$verifyStatus）")
        }
    }

    # 单向 CHAP
    $chapConfigured = $false
    if ($ChapUser -and $ChapSecret) {
        $step = 'set_chap'
        $chapSplat = New-ChapCredentialSplat -Parameters $setCommand.Parameters -User $ChapUser -Secret $ChapSecret
        Set-IscsiServerTarget -TargetName $TargetName -EnableChap:$true @chapSplat -ErrorAction Stop
        $chapConfigured = $true
    }

    # 反向 CHAP（必须先配置单向 CHAP）
    $reverseChapConfigured = $false
    if ($EnableReverseChap -eq 'true') {
        if (-not $chapConfigured) { throw '启用反向 CHAP 前必须先配置单向 CHAP' }
        if (-not ($ReverseChapUser -and $ReverseChapSecret)) {
            throw '启用反向 CHAP 必须同时提供 ReverseChapUser 与 ReverseChapSecret'
        }
        $step = 'set_reverse_chap'
        $reverseSplat = New-ReverseChapCredentialSplat -Parameters $setCommand.Parameters -User $ReverseChapUser -Secret $ReverseChapSecret
        Set-IscsiServerTarget -TargetName $TargetName -EnableReverseChap:$true @reverseSplat -ErrorAction Stop
        $reverseChapConfigured = $true
    }

    [pscustomobject]@{
        ok                    = $true
        step                  = 'done'
        action                = $action
        target_name           = $TargetName
        initiator_ids_updated = [bool]($InitiatorIdsJson -ne '')
        # 授权列表实际采用的绑定方式：string（原样传字符串）/ object:<类型名>（反射构造）
        initiator_bind        = $bindMode
        chap_enabled          = $chapConfigured
        reverse_chap_enabled  = $reverseChapConfigured
    } | ConvertTo-Json -Compress -Depth 5
} catch {
    # step 让上层日志能直接看出是哪一步失败（如 set_enabled / set_chap / create_target）。
    # error_type / inner 用于进一步区分：CimException 多为服务/名称校验问题，
    # ParameterBindingValidationException 则是 cmdlet 参数不合法。
    $inner = ''
    try {
        if ($_.Exception.InnerException) { $inner = $_.Exception.InnerException.Message }
    } catch { }
    [pscustomobject]@{
        ok         = $false
        reason     = 'set_target_failed'
        step       = $step
        error_type = [string]$_.Exception.GetType().Name
        inner      = [string]$inner
        message    = $_.Exception.Message
    } | ConvertTo-Json -Compress -Depth 5
    exit 1
}
