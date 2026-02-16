<?php

namespace App\Repository;

use App\Entity\Setting;
use Doctrine\Bundle\DoctrineBundle\Repository\ServiceEntityRepository;
use Doctrine\Persistence\ManagerRegistry;

/**
 * @extends ServiceEntityRepository<Setting>
 */
class SettingRepository extends ServiceEntityRepository
{
    public function __construct(ManagerRegistry $registry)
    {
        parent::__construct($registry, Setting::class);
    }

    /**
     * @return array<string, string|null>
     */
    public function getPublicSettings(): array
    {
        $settings = $this->findBy(['isPublic' => true]);
        $result = [];
        foreach ($settings as $setting) {
            $result[$setting->getKey()] = $setting->getValue();
        }
        return $result;
    }

    /**
     * @return array<string, string|null>
     */
    public function getAllSettings(): array
    {
        $settings = $this->findAll();
        $result = [];
        foreach ($settings as $setting) {
            $result[$setting->getKey()] = $setting->getValue();
        }
        return $result;
    }
}
