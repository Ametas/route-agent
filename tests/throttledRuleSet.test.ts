import { describe, it, beforeEach, afterEach } from 'node:test';
import assert from 'node:assert/strict';
import { promises as fs } from 'node:fs';
import path from 'node:path';
import os from 'node:os';

/**
 * Файл rule-set'а со списком задушенных префиксов (2026-08-30).
 *
 * Проверяются два свойства, каждое из которых уже один раз стоило бы дорого:
 *  - файл СОЗДАЁТСЯ до проверки конфига. sing-box читает локальный rule-set при создании и
 *    отвергает конфиг, если файла нет — значит `sing-box check` не прошёл бы, применение
 *    откатилось, и нода молча осталась бы на старом конфиге (ровно как было с `v2ray_api`);
 *  - файл НЕ переписывается тем же содержимым. За ним следит `fswatch`, и он перечитывает правила
 *    на каждую запись; переписывание одинакового списка дёргало бы перезагрузку правил впустую.
 */

const SANDBOX = await fs.mkdtemp(path.join(os.tmpdir(), 'ruleset-agent-'));
process.env.SINGBOX_CONFIG_PATH = path.join(SANDBOX, 'config.json');

const { throttledRuleSetPath, ensureThrottledRuleSetFile, writeThrottledRuleSet } = await import(
  '../src/utils/throttledRuleSet.js'
);

const doc = (cidrs: string[]): string =>
  JSON.stringify({ version: 1, rules: [{ source_ip_cidr: cidrs }] });

describe('файл rule-set задушенных префиксов', () => {
  beforeEach(async () => {
    await fs.rm(throttledRuleSetPath(), { force: true });
  });

  afterEach(async () => {
    await fs.rm(throttledRuleSetPath(), { force: true });
  });

  describe('создание заполнителя', () => {
    it('создаёт файл, если его нет', async () => {
      await ensureThrottledRuleSetFile();

      const parsed = JSON.parse(await fs.readFile(throttledRuleSetPath(), 'utf-8'));
      assert.equal(parsed.rules.length, 1, 'sing-box отвергает набор без правил');
      assert.deepEqual(parsed.rules[0].source_ip_cidr, ['192.0.2.1/32']);
    });

    it('заполнитель не трогает петлю', async () => {
      // На 127.0.0.1 приходят локальные инбаунды диспетчера — правило по ней задушило бы
      // собственный транзит ноды.
      await ensureThrottledRuleSetFile();

      assert.ok(!(await fs.readFile(throttledRuleSetPath(), 'utf-8')).includes('127.0.0.1'));
    });

    it('не затирает уже доставленный список', async () => {
      // Иначе каждый пуш конфига сбрасывал бы действующие шейпы обратно в пустоту.
      await writeThrottledRuleSet(doc(['203.0.113.0/24']));
      await ensureThrottledRuleSetFile();

      const parsed = JSON.parse(await fs.readFile(throttledRuleSetPath(), 'utf-8'));
      assert.deepEqual(parsed.rules[0].source_ip_cidr, ['203.0.113.0/24']);
    });
  });

  describe('доставка списка', () => {
    it('записывает новое содержимое и сообщает об изменении', async () => {
      const result = await writeThrottledRuleSet(doc(['203.0.113.0/24']));

      assert.equal(result.changed, true);
      const parsed = JSON.parse(await fs.readFile(throttledRuleSetPath(), 'utf-8'));
      assert.deepEqual(parsed.rules[0].source_ip_cidr, ['203.0.113.0/24']);
    });

    it('одинаковое содержимое не переписывает файл', async () => {
      await writeThrottledRuleSet(doc(['203.0.113.0/24']));
      const before = (await fs.stat(throttledRuleSetPath())).mtimeMs;

      const result = await writeThrottledRuleSet(doc(['203.0.113.0/24']));

      assert.equal(result.changed, false);
      assert.equal((await fs.stat(throttledRuleSetPath())).mtimeMs, before, 'файл не должен быть тронут');
    });

    it('различия в форматировании отправителя не считаются изменением', async () => {
      // Сравнение идёт по разобранному документу, а не по байтам присланной строки — иначе смена
      // отступов на нашей стороне дёргала бы перезагрузку правил на всём флоте.
      await writeThrottledRuleSet(doc(['203.0.113.0/24']));

      const pretty = JSON.stringify({ version: 1, rules: [{ source_ip_cidr: ['203.0.113.0/24'] }] }, null, 4);
      assert.equal((await writeThrottledRuleSet(pretty)).changed, false);
    });

    it('пустой набор правил отвергается, а не пишется', async () => {
      // sing-box такой файл не примет, и наблюдатель отрапортует ошибкой перезагрузки — нода
      // осталась бы со старым списком без внятной причины.
      await assert.rejects(() => writeThrottledRuleSet(JSON.stringify({ version: 1, rules: [] })));
    });

    it('не-JSON отвергается', async () => {
      await assert.rejects(() => writeThrottledRuleSet('это не json'));
    });

    it('после отвергнутого документа прежний список остаётся на месте', async () => {
      await writeThrottledRuleSet(doc(['203.0.113.0/24']));
      await writeThrottledRuleSet(JSON.stringify({ version: 1, rules: [] })).catch(() => {});

      const parsed = JSON.parse(await fs.readFile(throttledRuleSetPath(), 'utf-8'));
      assert.deepEqual(parsed.rules[0].source_ip_cidr, ['203.0.113.0/24']);
    });
  });
});
