<script setup lang="ts">
import { computed, onMounted, reactive, ref } from 'vue';
import { Plus, Refresh, Search } from '@element-plus/icons-vue';
import type { DomainRecord, EntityConfig } from '../types/domain';
import { allowedTransitions, ALL_GATE_STATE } from '../types/status';
import { formatDate, riskLabel, statusLabel } from '../utils/format';
import { useAuth } from '../hooks/useAuth';
import { usePolling } from '../hooks/usePolling';
import { request } from '../api/client';
import StatusBadge from './common/StatusBadge.vue';
import GateStateBadge from './common/GateStateBadge.vue';
import MetricCard from './common/MetricCard.vue';
import DirectiveTimeline from './common/DirectiveTimeline.vue';
import ConfirmDialog from './common/ConfirmDialog.vue';

const props = defineProps<{ config: EntityConfig; store: any }>();
const { session, can } = useAuth();
const search = ref('');
const showCreate = ref(false);
const editingId = ref<number | null>(null);
const directiveItems = ref<DomainRecord[]>([]);
const pending = ref<{ item: DomainRecord; status: string } | null>(null);
const transitionReason = ref('');
const relatedOptions = ref<DomainRecord[]>([]);
const createForm = reactive({
  code: '', name: '', description: '', facility: '', owner: '', category: '',
  riskLevel: 'medium', metricValue: 0, metricUnit: '%', evidence: '', relatedCode: '', gateState: 'closed',
  measuredGateState: 'closed', observedAt: '',
});

const highRisk = computed(() => props.store.items.filter((item: DomainRecord) => ['high', 'critical'].includes(item.riskLevel)).length);
const canCreate = computed(() => can('operator', 'admin'));
const pageDescription = computed(() => ({
  reservoir: '监控水位阈值与许可窗口，为调度决策提供约束。',
  gateUnit: '查看闸门实时状态，所有开闭动作必须经过中间态。',
  operationDirective: '编排闸门指令，并由不同账号完成提交与安全复核。',
  executionConfirmation: '登记现场实测闸位与观测时间；确认时重新读取闸门，只有实测值等于指令目标、观测时间晚于开始执行且当前闸门状态与实测值一致，指令才会完成。',
}[props.config.key] || `管理${props.config.label}状态、风险与责任人。`));

async function load(): Promise<void> {
  await props.store.load(props.config.path, search.value);
  if (props.config.key === 'executionConfirmation') {
    try {
      const result = await request<DomainRecord[]>('/directives?page=1&pageSize=100');
      directiveItems.value = result.data;
    } catch {
      directiveItems.value = [];
    }
  }
}

const directiveTargetOf = (code?: string) => directiveItems.value.find((d) => d.code === code)?.gateState;

const pendingDirective = computed<DomainRecord | undefined>(() =>
  pending.value?.item.relatedCode
    ? directiveItems.value.find((d) => d.code === pending.value?.item.relatedCode)
    : undefined);

onMounted(() => void load());
usePolling(load, 30_000);

const metricLabel = computed(() => ({
	reservoir: '当前水位', gateUnit: '当前开度', operationDirective: '目标开度', executionConfirmation: '实际开度',
}[props.config.key] || '指标值'));

const relationLabel = computed(() => ({ gateUnit: '所属库区', operationDirective: '目标闸门', executionConfirmation: '关联指令' }[props.config.key] || '关联对象'));

const createReady = computed(() => Boolean(
	createForm.code.trim() && createForm.name.trim() && createForm.facility.trim() && createForm.owner.trim() &&
	createForm.category.trim() && createForm.evidence.trim() &&
	(!['gateUnit', 'operationDirective', 'executionConfirmation'].includes(props.config.key) || createForm.relatedCode) &&
	(props.config.key !== 'executionConfirmation' || (createForm.measuredGateState && createForm.observedAt)),
));

async function prepareCreate(): Promise<void> {
  const suffix = String(Date.now()).slice(-6);
  createForm.code = `${props.config.key.replace(/[a-z]/g, (value) => value.toUpperCase()).slice(0, 3)}-${suffix}`;
	createForm.name = '';
	createForm.description = '';
	createForm.facility = '';
  createForm.owner = session.value?.displayName || '现场操作员';
	createForm.category = '';
	createForm.metricValue = 0;
	createForm.metricUnit = props.config.key === 'reservoir' ? 'm' : '%';
	createForm.evidence = '';
	createForm.relatedCode = '';
  createForm.gateState = 'closed';
	createForm.measuredGateState = 'closed';
	createForm.observedAt = '';
	editingId.value = null;
	relatedOptions.value = [];
	const relationPaths: Record<string, string> = { gateUnit: 'reservoirs', operationDirective: 'gates', executionConfirmation: 'directives' };
	try {
		const relationPath = relationPaths[props.config.key];
		if (relationPath) {
			const result = await request<DomainRecord[]>(`/${relationPath}?page=1&pageSize=100`);
			relatedOptions.value = result.data.filter((item) =>
				props.config.key === 'operationDirective' ? item.status !== 'locked' :
				props.config.key === 'executionConfirmation' ? item.status === 'executing' : true,
			);
			selectRelated(relatedOptions.value[0]?.code || '');
		}
		showCreate.value = true;
	} catch (reason) {
		props.store.error = reason instanceof Error ? reason.message : String(reason);
	}
}

function selectRelated(code: string): void {
	createForm.relatedCode = code;
	const related = relatedOptions.value.find((item) => item.code === code);
	if (related) {
		createForm.facility = related.facility;
		// A receipt registers the on-site measurement of the directive target.
		if (props.config.key === 'executionConfirmation' && related.gateState) {
			createForm.measuredGateState = related.gateState;
		}
	}
}

const editingVersion = ref(0);

function toLocalInput(value: string): string {
	const date = new Date(value);
	if (Number.isNaN(date.getTime())) return '';
	const pad = (n: number) => String(n).padStart(2, '0');
	return `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}T${pad(date.getHours())}:${pad(date.getMinutes())}`;
}

async function prepareEdit(item: DomainRecord): Promise<void> {
	await prepareCreate();
	editingId.value = item.id;
	editingVersion.value = item.version;
	createForm.code = item.code;
	createForm.name = item.name;
	createForm.description = item.description;
	createForm.facility = item.facility;
	createForm.owner = item.owner;
	createForm.category = item.category;
	createForm.riskLevel = item.riskLevel;
	createForm.metricValue = item.metricValue;
	createForm.metricUnit = item.metricUnit;
	createForm.evidence = item.evidence;
	selectRelated(item.relatedCode);
	createForm.measuredGateState = (item.measuredGateState || item.gateState || 'closed') as string;
	createForm.observedAt = item.observedAt ? toLocalInput(item.observedAt) : '';
}

async function createRecord(): Promise<void> {
	if (!createReady.value) {
		props.store.error = props.config.key === 'executionConfirmation'
			? '请完整填写必填业务字段、现场证据、实测闸位和观测时间'
			: '请完整填写必填业务字段和现场证据';
		return;
	}
	const payload = {
		...createForm,
		effectiveAt: new Date().toISOString(),
		observedAt: createForm.observedAt ? new Date(createForm.observedAt).toISOString() : undefined,
	};
	if (editingId.value === null) {
		await props.store.createRecord(props.config.path, payload);
	} else {
		await props.store.updateRecord(props.config.path, editingId.value, { ...payload, expectedVersion: editingVersion.value });
	}
	if (!props.store.error) showCreate.value = false;
}

function transitionsFor(item: DomainRecord): readonly string[] {
  return allowedTransitions(props.config.key, item.status).filter((target) => {
    if (props.config.key !== 'operationDirective') return can('operator', 'admin');
	if (target === 'completed') return false;
    if (target === 'pending' || target === 'executing' || target === 'completed') return can('operator', 'admin');
    if (target === 'approved') return can('reviewer', 'admin') && item.submittedBy !== session.value?.username;
    if (target === 'aborted') return can('operator', 'reviewer', 'admin');
    return false;
  });
}

function selectTransition(item: DomainRecord, status: string): void {
  pending.value = { item, status };
  transitionReason.value = status === 'approved'
    ? '已复核闸门目标、水位窗口、设备闭锁和现场证据'
    : status === 'confirmed'
      ? '现场实测闸位与指令目标一致，观测时间晚于开始执行，重新读取闸门状态与实测值相同'
      : `值班人员确认将状态由 ${item.status} 推进至 ${status}`;
}

async function confirmTransition(): Promise<void> {
  if (!pending.value || transitionReason.value.trim().length < 3) return;
  const { id } = pending.value.item;
  const status = pending.value.status;
  await props.store.transition(props.config.path, pending.value.item, status, transitionReason.value.trim());
  if (!props.store.error) {
    pending.value = null;
  } else {
    // A failed verification bumps the receipt version and records its reason;
    // rebind the dialog to the refreshed row so a retry uses current data.
    const refreshed = props.store.items.find((item: DomainRecord) => item.id === id);
    if (refreshed) pending.value = { item: refreshed, status };
  }
}
</script>

<template>
  <main class="workspace">
    <header class="page-header">
      <div><p class="eyebrow">业务工作台</p><h1>{{ config.label }}</h1><p>{{ pageDescription }}</p></div>
      <el-button v-if="canCreate" type="primary" :icon="Plus" @click="prepareCreate">新增{{ config.label }}</el-button>
    </header>

    <section class="metrics" aria-label="业务统计">
      <MetricCard label="记录总数" :value="store.meta.total" detail="当前筛选范围" />
      <MetricCard label="高风险" :value="highRisk" detail="需要优先复核" />
      <MetricCard label="状态种类" :value="new Set(store.items.map((item: DomainRecord) => item.status)).size" detail="当前状态覆盖" />
    </section>

    <DirectiveTimeline
      v-if="['operationDirective', 'executionConfirmation'].includes(config.key)"
      :records="store.items"
      :kind="config.key"
    />

    <section class="toolbar" aria-label="筛选工具栏">
      <el-input v-model="search" :prefix-icon="Search" :placeholder="`搜索${config.label}编码或名称`" clearable @keyup.enter="load" />
      <el-button type="primary" :icon="Search" @click="load">查询</el-button>
      <el-button :icon="Refresh" @click="search = ''; load()">重置</el-button>
    </section>
    <el-alert v-if="store.error" :title="store.error" type="error" show-icon closable @close="store.error = ''" />

    <section class="table-shell">
      <el-table v-loading="store.loading" :data="store.items" empty-text="暂无符合条件的记录">
        <el-table-column prop="code" label="编码" width="130" />
        <el-table-column label="名称" min-width="190">
          <template #default="{ row }"><strong>{{ row.name }}</strong><small>{{ row.facility }}</small></template>
        </el-table-column>
        <el-table-column label="状态" width="130">
          <template #default="{ row }">
            <GateStateBadge v-if="config.key === 'gateUnit'" :state="row.status" />
            <StatusBadge v-else :status="row.status" />
          </template>
        </el-table-column>
		<el-table-column v-if="config.key === 'operationDirective'" label="目标状态" width="120">
          <template #default="{ row }"><GateStateBadge :state="row.gateState || 'closed'" /></template>
        </el-table-column>
		<el-table-column v-if="config.key === 'executionConfirmation'" label="实测闸位" width="110">
          <template #default="{ row }"><GateStateBadge v-if="row.measuredGateState" :state="row.measuredGateState" /><span v-else class="muted">未登记</span></template>
        </el-table-column>
		<el-table-column v-if="config.key === 'executionConfirmation'" label="观测时间" width="165">
          <template #default="{ row }">{{ row.observedAt ? formatDate(row.observedAt) : '-' }}</template>
        </el-table-column>
		<el-table-column v-if="config.key === 'executionConfirmation'" label="校验结果" min-width="220">
          <template #default="{ row }">
            <el-tag v-if="row.verifyStatus === 'passed'" type="success" size="small">校验通过</el-tag>
            <el-tooltip v-else-if="row.verifyStatus === 'failed'" :content="row.verifyDetail" placement="top" :show-after="200">
              <el-tag type="danger" size="small">校验未通过</el-tag>
            </el-tooltip>
            <el-tag v-else type="info" size="small">待确认校验</el-tag>
            <small v-if="row.verifyStatus === 'failed'" class="verify-detail">{{ row.verifyDetail }}</small>
          </template>
        </el-table-column>
		<el-table-column label="风险" width="80"><template #default="{ row }">{{ riskLabel(row.riskLevel) }}</template></el-table-column>
        <el-table-column prop="owner" label="责任人" min-width="110" />
		<el-table-column v-if="['gateUnit', 'operationDirective', 'executionConfirmation'].includes(config.key)" prop="relatedCode" :label="relationLabel" width="130" />
        <el-table-column label="指标" width="105"><template #default="{ row }">{{ row.metricValue }} {{ row.metricUnit }}</template></el-table-column>
        <el-table-column label="更新时间" width="165"><template #default="{ row }">{{ formatDate(row.updatedAt) }}</template></el-table-column>
        <el-table-column label="操作" min-width="220" fixed="right">
          <template #default="{ row }">
            <div class="row-actions">
			  <el-button v-for="target in transitionsFor(row)" :key="target" link type="primary" @click="selectTransition(row, target)">推进至{{ statusLabel(target) }}</el-button>
			  <el-button v-if="config.key === 'executionConfirmation' && row.status === 'pending' && can('operator', 'admin')" link type="warning" @click="prepareEdit(row)">补正回执</el-button>
            </div>
            <span v-if="!transitionsFor(row).length && !(config.key === 'executionConfirmation' && row.status === 'pending' && can('operator', 'admin'))" class="muted">当前角色无可执行动作</span>
          </template>
        </el-table-column>
      </el-table>
    </section>

    <ConfirmDialog v-model="showCreate" :title="editingId === null ? `新增${config.label}` : `补正${config.label}`" :confirm-label="editingId === null ? '创建记录' : '保存补正'" :confirm-disabled="!createReady" :loading="store.loading" @confirm="createRecord">
      <el-form class="record-form" label-position="top">
		<el-alert v-if="config.key === 'executionConfirmation'" title="回执只登记现场实测闸位和观测时间；确认时服务端会重新读取闸门，不会由确认接口改写闸门。三项校验任一不符，回执保持待确认、指令保持执行中。" type="info" show-icon :closable="false" />
		<el-alert v-if="['gateUnit', 'operationDirective', 'executionConfirmation'].includes(config.key) && !relatedOptions.length" :title="`当前没有可用的${relationLabel}`" type="warning" show-icon />
        <div class="form-grid">
          <el-form-item label="业务编码"><el-input v-model="createForm.code" :disabled="editingId !== null" /></el-form-item>
          <el-form-item label="名称"><el-input v-model="createForm.name" /></el-form-item>
		  <el-form-item v-if="['gateUnit', 'operationDirective', 'executionConfirmation'].includes(config.key)" :label="relationLabel">
			<el-select :model-value="createForm.relatedCode" filterable :disabled="editingId !== null" @update:model-value="selectRelated">
				<el-option v-for="item in relatedOptions" :key="item.id" :label="`${item.code} · ${item.name}`" :value="item.code" />
			</el-select>
		  </el-form-item>
		  <el-form-item label="作业区域"><el-input v-model="createForm.facility" :disabled="['gateUnit', 'operationDirective', 'executionConfirmation'].includes(config.key)" /></el-form-item>
          <el-form-item label="责任人"><el-input v-model="createForm.owner" /></el-form-item>
		  <el-form-item label="业务类别"><el-input v-model="createForm.category" /></el-form-item>
		  <el-form-item label="风险等级"><el-select v-model="createForm.riskLevel"><el-option v-for="risk in ['low', 'medium', 'high', 'critical']" :key="risk" :label="riskLabel(risk)" :value="risk" /></el-select></el-form-item>
		  <el-form-item :label="metricLabel"><el-input-number v-model="createForm.metricValue" :min="0" :precision="2" controls-position="right" /></el-form-item>
		  <el-form-item label="指标单位"><el-input v-model="createForm.metricUnit" /></el-form-item>
		  <el-form-item v-if="config.key === 'operationDirective'" label="目标闸门状态"><el-select v-model="createForm.gateState"><el-option v-for="state in ['open', 'closed', 'locked']" :key="state" :label="statusLabel(state)" :value="state" /></el-select></el-form-item>
		  <el-form-item v-if="config.key === 'executionConfirmation'" label="现场实测闸位">
			<el-select v-model="createForm.measuredGateState">
				<el-option v-for="state in ALL_GATE_STATE" :key="state" :label="statusLabel(state)" :value="state" />
			</el-select>
		  </el-form-item>
		  <el-form-item v-if="config.key === 'executionConfirmation'" label="观测时间（须晚于指令开始执行时间）">
			<el-date-picker v-model="createForm.observedAt" type="datetime" format="YYYY-MM-DD HH:mm" value-format="YYYY-MM-DDTHH:mm" placeholder="选择现场观测时间" class="full-width" />
		  </el-form-item>
        </div>
		<el-form-item label="业务说明"><el-input v-model="createForm.description" type="textarea" :rows="2" maxlength="1000" show-word-limit /></el-form-item>
        <el-form-item label="现场证据"><el-input v-model="createForm.evidence" type="textarea" :rows="3" /></el-form-item>
      </el-form>
    </ConfirmDialog>

    <ConfirmDialog :model-value="Boolean(pending)" title="确认状态迁移" confirm-label="确认并记录审计" @update:model-value="pending = null" @confirm="confirmTransition">
      <p>此次操作会校验角色和版本，并将状态、请求 ID 与审计证据原子写入。</p>
      <div class="transition-summary"><StatusBadge :status="pending?.item.status || ''" /><span>到</span><StatusBadge :status="pending?.status || ''" /></div>
      <template v-if="config.key === 'executionConfirmation' && pending?.status === 'confirmed'">
        <el-alert
          title="确认时服务端会重新读取关联指令和闸门，确认接口不会改写闸门。以下三项全部满足才会完成指令，任一不符都保留待确认回执和执行中指令。"
          type="info" show-icon :closable="false" class="verify-rules"
        />
        <ul class="verify-checklist">
          <li :class="{ 'verify-ok': pending?.item.measuredGateState === pendingDirective?.gateState, 'verify-bad': pending?.item.measuredGateState && pending?.item.measuredGateState !== pendingDirective?.gateState }">
            实测闸位
            <GateStateBadge v-if="pending?.item.measuredGateState" :state="pending.item.measuredGateState" />
            <span v-else>未登记</span>
            等于指令目标
            <GateStateBadge v-if="pendingDirective?.gateState" :state="pendingDirective.gateState" />
            <span v-else>（指令目标缺失）</span>
          </li>
          <li :class="{ 'verify-ok': pending?.item.observedAt }">
            观测时间{{ pending?.item.observedAt ? formatDate(pending.item.observedAt) : '未登记' }}，须晚于开始执行{{ pendingDirective?.executedAt ? formatDate(pendingDirective.executedAt) : '（缺少执行时间）' }}
          </li>
          <li>确认瞬间重新读取闸门，当前状态必须与实测值相同（闸位由现场设备反馈，不由本接口写入）</li>
        </ul>
      </template>
      <el-alert v-if="pending?.item.verifyStatus === 'failed'" :title="`上次校验未通过：${pending.item.verifyDetail}`" type="error" show-icon :closable="false" class="verify-rules" />
      <el-alert v-if="store.error" :title="store.error" type="error" show-icon :closable="false" class="verify-rules" />
      <el-input v-model="transitionReason" type="textarea" :rows="3" maxlength="500" show-word-limit aria-label="迁移原因" />
    </ConfirmDialog>
  </main>
</template>
