<?php

namespace App\Service;

use App\Entity\Setting;
use App\Repository\SettingRepository;
use Doctrine\ORM\EntityManagerInterface;

class SettingsService
{
    public function __construct(
        private EntityManagerInterface $entityManager,
        private SettingRepository $settingRepository
    ) {}

    public function get(string $key, ?string $default = null): ?string
    {
        $setting = $this->settingRepository->find($key);
        if ($setting === null) {
            return $default;
        }
        return $setting->getValue() ?? $default;
    }

    public function set(string $key, ?string $value): void
    {
        $setting = $this->settingRepository->find($key);
        if ($setting === null) {
            $setting = new Setting($key, $value);
            $this->entityManager->persist($setting);
        } else {
            $setting->setValue($value);
        }
        $this->entityManager->flush();
    }

    /**
     * @return array<string, string|null>
     */
    public function getPublicSettings(): array
    {
        return $this->settingRepository->getPublicSettings();
    }

    /**
     * @return array<string, string|null>
     */
    public function getAllSettings(): array
    {
        return $this->settingRepository->getAllSettings();
    }
}
